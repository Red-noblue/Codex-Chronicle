// 中文说明：读取脱敏真实结构 fixture 的只读关系快照；schema、路径和引用证据不满足时 fail-closed。
package compat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"codex-chronicle.local/internal/domain"
)

const (
	RealShapeFixtureKind           = "codex-realshape-fixture-v1"
	RealShapeCopyKind              = "codex-real-copy-v1"
	RealShapeFixtureSchemaSHA256   = "92f046b914b2ef4d4ba8ae3a2f1840b776338b0391a1a8d52a1eb83ad2fde36f"
	RealShapeSQLiteSemanticsSHA256 = "bb9cab8d75d076f7e20a0e3be845c4e204641cf02bee6e54309aae110677a53e"
	// RealCodex0166SchemaSHA256 和 RealCodex0166SQLiteSemanticsSHA256 来自隔离 Codex 0.166.0 的完整六库快照。
	RealCodex0166SchemaSHA256                = "d3a8deb2a32be2e526eea7e8c9bc3966c8f977cc3931a4d789df033cce902774"
	RealCodex0166SQLiteSemanticsSHA256       = "d5749b9a980feb24e2e97aedcd5565a0af20b503f910f4b795473b30cb73d813"
	realShapeFixtureMarker                   = "fixture-only.json"
	realShapeCopyMarker                      = "codex-real-copy-only.json"
	RealShapeCopyMarker                      = realShapeCopyMarker
	realShapeSessionIndex                    = "session_index.jsonl"
	maxRealShapeRowsPerReference       int64 = 4096
)

var realShapeDatabases = []string{
	"goals_1.sqlite",
	"logs_2.sqlite",
	"memories_1.sqlite",
	"queue_1.sqlite",
	"state_5.sqlite",
	"thread_history_1.sqlite",
}

// RealShapeFixtureManifest 是 fixture 生成器写入的信任边界标记。
type RealShapeFixtureManifest struct {
	Kind                  string `json:"kind"`
	SchemaSHA256          string `json:"schema_sha256"`
	SQLiteSemanticsSHA256 string `json:"sqlite_semantics_sha256"`
	// SourceRoot 是稳定副本记录的源 .codex 根，仅用于校验绝对 rollout_path 的受限映射。
	SourceRoot        string                     `json:"source_root,omitempty"`
	CompletenessState string                     `json:"completeness_state,omitempty"`
	ControlPlane      *RealShapeCopyControlPlane `json:"control_plane,omitempty"`
}

// RealShapeCopyControlPlane 是稳定副本 manifest 中不含正文的控制面完整性摘要。
type RealShapeCopyControlPlane struct {
	SchemaSemanticSHA256 string `json:"schema_semantic_sha256,omitempty"`
	Complete             bool   `json:"complete"`
	CompletenessState    string `json:"completeness_state,omitempty"`
}

// RealShapeRowSnapshot 是一个数据库表中与目标 thread 关联的行级定位摘要。
// 它只保存主键/rowid，不保存正文列值；RowsComplete=false 时不能授权任何写操作。
type RealShapeRowSnapshot struct {
	Database     string                      `json:"database"`
	Table        string                      `json:"table"`
	Column       string                      `json:"column"`
	Relation     string                      `json:"relation"`
	RowCount     int64                       `json:"row_count"`
	RowKeys      []domain.SQLiteRowReference `json:"row_keys,omitempty"`
	RowsComplete bool                        `json:"rows_complete"`
}

// RealShapeFixtureSnapshot 是 Go 兼容层对脱敏真实形状副本的只读预览。
type RealShapeFixtureSnapshot struct {
	Root                 string                 `json:"root"`
	ThreadID             string                 `json:"thread_id"`
	RolloutPath          string                 `json:"rollout_path"`
	RolloutSHA256        string                 `json:"rollout_sha256"`
	SessionIndexLines    []uint64               `json:"session_index_lines"`
	Databases            []domain.SQLiteSummary `json:"databases"`
	Rows                 []RealShapeRowSnapshot `json:"rows"`
	SchemaSemanticSHA256 string                 `json:"schema_semantic_sha256"`
	Complete             bool                   `json:"complete"`
	Diagnostics          []domain.Diagnostic    `json:"diagnostics,omitempty"`
	ReadOnly             bool                   `json:"read_only"`
}

// InspectRealShapeFixture 只读取由 codex_realshape_fixture.py 生成的脱敏副本。
// 它不修改文件、SQLite、WAL 或索引；返回的快照不能直接授权生命周期写入。
func InspectRealShapeFixture(ctx context.Context, root, threadID string) (RealShapeFixtureSnapshot, error) {
	return inspectRealShape(ctx, root, threadID, realShapeFixtureMarker, RealShapeFixtureKind, "fixture")
}

// InspectRealShapeCopy 只读取带 codex-real-copy-v1 标记的真实结构副本。
// 副本仍然只读；生命周期写入必须由 RealShapeCopyMutation 进一步绑定快照。
func InspectRealShapeCopy(ctx context.Context, root, threadID string) (RealShapeFixtureSnapshot, error) {
	if pathHasCodexComponent(root) {
		return RealShapeFixtureSnapshot{Root: root, ThreadID: strings.TrimSpace(threadID), ReadOnly: true}, fmt.Errorf("真实结构副本预览拒绝 .codex 根目录")
	}
	return inspectRealShape(ctx, root, threadID, realShapeCopyMarker, RealShapeCopyKind, "副本")
}

func inspectRealShape(ctx context.Context, root, threadID, markerName, markerKind, label string) (RealShapeFixtureSnapshot, error) {
	snapshot := RealShapeFixtureSnapshot{Root: root, ThreadID: strings.TrimSpace(threadID), Complete: true, ReadOnly: true, Databases: make([]domain.SQLiteSummary, 0), Rows: make([]RealShapeRowSnapshot, 0), SessionIndexLines: make([]uint64, 0), Diagnostics: make([]domain.Diagnostic, 0)}
	if snapshot.ThreadID == "" {
		return snapshot, fmt.Errorf("真实结构%s预览必须包含 thread ID", label)
	}
	absRoot, err := realShapeRoot(root)
	if err != nil {
		return snapshot, err
	}
	snapshot.Root = absRoot
	manifestPath, err := realShapeReadOnlyPath(absRoot, markerName)
	if err != nil {
		return snapshot, fmt.Errorf("真实结构%s标记不可安全定位: %w", label, err)
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return snapshot, fmt.Errorf("读取真实结构%s标记失败: %w", label, err)
	}
	var marker RealShapeFixtureManifest
	if err := json.Unmarshal(manifestBytes, &marker); err != nil {
		return snapshot, fmt.Errorf("真实结构%s标记不是有效 JSON: %w", label, err)
	}
	if marker.Kind != markerKind || !trustedRealShapeSchemaForKind(marker, markerKind) {
		return snapshot, fmt.Errorf("真实结构%s标记或 schema 摘要不受信任", label)
	}
	expectedSemantics := RealShapeSQLiteSemanticsSHA256
	if marker.SchemaSHA256 == RealCodex0166SchemaSHA256 {
		expectedSemantics = RealCodex0166SQLiteSemanticsSHA256
	}
	semanticHash, err := realShapeSQLiteSemantics(ctx, absRoot)
	if err != nil {
		return snapshot, err
	}
	snapshot.SchemaSemanticSHA256 = semanticHash
	if semanticHash != expectedSemantics {
		return snapshot, fmt.Errorf("SQLite 语义摘要不匹配: %s", semanticHash)
	}

	for _, databaseName := range realShapeDatabases {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		databasePath, pathErr := realShapeReadOnlyPath(absRoot, databaseName)
		if pathErr != nil {
			return snapshot, fmt.Errorf("数据库 %s 不可安全定位: %w", databaseName, pathErr)
		}
		summary, summaryErr := AnalyzeSQLitePath(ctx, databasePath)
		if summaryErr != nil {
			return snapshot, fmt.Errorf("读取数据库 %s 失败: %w", databaseName, summaryErr)
		}
		snapshot.Databases = append(snapshot.Databases, summary)
		rows, rowsErr := realShapeRows(ctx, databasePath, databaseName, snapshot.ThreadID)
		if rowsErr != nil {
			return snapshot, rowsErr
		}
		snapshot.Rows = append(snapshot.Rows, rows...)
	}

	statePath, err := realShapeReadOnlyPath(absRoot, "state_5.sqlite")
	if err != nil {
		return snapshot, err
	}
	rolloutPath, err := realShapeRolloutPath(ctx, statePath, absRoot, marker.SourceRoot, snapshot.ThreadID)
	if err != nil {
		return snapshot, err
	}
	snapshot.RolloutPath = rolloutPath
	absoluteRollout, err := realShapeReadOnlyPath(absRoot, rolloutPath)
	if err != nil {
		return snapshot, err
	}
	snapshot.RolloutSHA256, err = realShapeHashFile(ctx, absoluteRollout)
	if err != nil {
		return snapshot, fmt.Errorf("读取 rollout 摘要失败: %w", err)
	}
	if markerKind == RealShapeFixtureKind || markerKind == RealShapeCopyKind {
		indexPath, indexErr := realShapeReadOnlyPath(absRoot, realShapeSessionIndex)
		if indexErr != nil {
			if markerKind == RealShapeCopyKind && os.IsNotExist(indexErr) {
				indexPath = ""
			} else {
				return snapshot, indexErr
			}
		}
		if indexPath != "" {
			index, indexErr := AnalyzeSessionIndexPath(indexPath)
			if indexErr != nil {
				return snapshot, indexErr
			}
			for _, entry := range index.Entries {
				if entry.ID == snapshot.ThreadID {
					snapshot.SessionIndexLines = append(snapshot.SessionIndexLines, entry.Line)
				}
			}
			if index.InvalidJSONLines > 0 {
				snapshot.Complete = false
				snapshot.Diagnostics = append(snapshot.Diagnostics, domain.Diagnostic{Code: "REALSHAPE_INDEX_INVALID", Severity: domain.SeverityWarning, Message: "session index 存在无效行，只能保留只读快照", Evidence: fmt.Sprintf("无效行数 %d", index.InvalidJSONLines), ReadOnly: true})
			}
			if len(snapshot.SessionIndexLines) == 0 {
				return snapshot, fmt.Errorf("session index 缺少目标 thread: %s", snapshot.ThreadID)
			}
		}
	}
	for index := range snapshot.Rows {
		if !snapshot.Rows[index].RowsComplete {
			snapshot.Complete = false
			snapshot.Diagnostics = append(snapshot.Diagnostics, domain.Diagnostic{Code: "REALSHAPE_ROWS_INCOMPLETE", Severity: domain.SeverityBlock, Message: "数据库关系行无法完整定位，只能保留只读快照", Evidence: snapshot.Rows[index].Database + "." + snapshot.Rows[index].Table + "." + snapshot.Rows[index].Column, ReadOnly: true})
		}
	}
	sort.Slice(snapshot.Databases, func(i, j int) bool {
		return snapshot.Databases[i].SchemaFingerprint < snapshot.Databases[j].SchemaFingerprint
	})
	sort.Slice(snapshot.Rows, func(i, j int) bool {
		left := snapshot.Rows[i].Database + "\x00" + snapshot.Rows[i].Table + "\x00" + snapshot.Rows[i].Column
		right := snapshot.Rows[j].Database + "\x00" + snapshot.Rows[j].Table + "\x00" + snapshot.Rows[j].Column
		return left < right
	})
	return snapshot, nil
}

func realShapeRows(ctx context.Context, path, databaseName, threadID string) ([]RealShapeRowSnapshot, error) {
	database, err := openReadOnlySQLite(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("打开真实结构数据库 %s 失败: %w", databaseName, err)
	}
	defer database.Close()
	tables, err := realShapeTableNames(ctx, database)
	if err != nil {
		return nil, fmt.Errorf("读取真实结构数据库 %s 表清单失败: %w", databaseName, err)
	}
	result := make([]RealShapeRowSnapshot, 0)
	for _, table := range tables {
		columns, err := pragmaColumnInfo(ctx, database, table)
		if err != nil {
			return nil, err
		}
		for _, column := range columns {
			if column.Name != "thread_id" && column.Name != "parent_thread_id" && column.Name != "child_thread_id" && !(table == "threads" && column.Name == "id") {
				continue
			}
			var count int64
			query := `SELECT COUNT(*) FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column.Name) + `" = ?`
			if err := database.QueryRowContext(ctx, query, threadID).Scan(&count); err != nil {
				return nil, fmt.Errorf("读取真实结构关系 %s.%s 失败: %w", table, column.Name, err)
			}
			if count == 0 {
				continue
			}
			keys, complete := readRelationshipRowKeys(ctx, database, table, column.Name, threadID, count, columns)
			result = append(result, RealShapeRowSnapshot{Database: databaseName, Table: table, Column: column.Name, Relation: relationshipName(table, column.Name), RowCount: count, RowKeys: keys, RowsComplete: complete && count <= maxRealShapeRowsPerReference})
		}
	}
	return result, nil
}

func realShapeTableNames(ctx context.Context, database interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]string, error) {
	rows, err := database.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func realShapeRolloutPath(ctx context.Context, statePath, root, sourceRoot, threadID string) (string, error) {
	database, err := openReadOnlySQLite(ctx, statePath)
	if err != nil {
		return "", err
	}
	defer database.Close()
	var rollout string
	if err := database.QueryRowContext(ctx, `SELECT rollout_path FROM "threads" WHERE "id" = ?`, threadID).Scan(&rollout); err != nil {
		return "", fmt.Errorf("读取目标 thread rollout_path 失败: %w", err)
	}
	clean, safe := resolveRolloutPath(root, sourceRoot, rollout)
	if !safe {
		return "", fmt.Errorf("目标 rollout 路径不安全: %s", rollout)
	}
	if _, err := realShapeReadOnlyPath(root, clean); err != nil {
		return "", fmt.Errorf("目标 rollout 不存在或不安全: %w", err)
	}
	return clean, nil
}

// ResolveRolloutPath 将 Codex SQLite 中的 rollout_path 安全映射为扫描根内的相对路径。
// 稳定副本允许使用 manifest 记录的源根映射，其他绝对路径仍然拒绝。
func ResolveRolloutPath(root, value string) (string, bool) {
	sourceRoot := ""
	if filepath.IsAbs(filepath.FromSlash(strings.TrimSpace(value))) {
		if absoluteRoot, rootErr := filepath.Abs(root); rootErr == nil {
			if _, inside := relativeWithin(absoluteRoot, filepath.Clean(filepath.FromSlash(value))); !inside {
				if marker, markerErr := readRealCopyManifest(root); markerErr == nil {
					sourceRoot = marker.SourceRoot
				}
			}
		}
	}
	return resolveRolloutPath(root, sourceRoot, value)
}

func readRealCopyManifest(root string) (RealShapeFixtureManifest, error) {
	path, err := realShapeReadOnlyPath(root, realShapeCopyMarker)
	if err != nil {
		return RealShapeFixtureManifest{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return RealShapeFixtureManifest{}, err
	}
	var marker RealShapeFixtureManifest
	if err := json.Unmarshal(data, &marker); err != nil {
		return RealShapeFixtureManifest{}, err
	}
	if marker.Kind != RealShapeCopyKind || !trustedRealShapeSchema(marker) || strings.TrimSpace(marker.SourceRoot) == "" {
		return RealShapeFixtureManifest{}, fmt.Errorf("稳定副本 manifest 未提供受信任的源根映射")
	}
	return marker, nil
}

func trustedRealShapeSchema(marker RealShapeFixtureManifest) bool {
	return (marker.SchemaSHA256 == RealShapeFixtureSchemaSHA256 && marker.SQLiteSemanticsSHA256 == RealShapeSQLiteSemanticsSHA256) ||
		(marker.SchemaSHA256 == RealCodex0166SchemaSHA256 && marker.SQLiteSemanticsSHA256 == RealCodex0166SQLiteSemanticsSHA256)
}

func trustedRealShapeSchemaForKind(marker RealShapeFixtureManifest, kind string) bool {
	switch kind {
	case RealShapeFixtureKind:
		return marker.SchemaSHA256 == RealShapeFixtureSchemaSHA256 && marker.SQLiteSemanticsSHA256 == RealShapeSQLiteSemanticsSHA256
	case RealShapeCopyKind:
		return trustedRealShapeSchema(marker)
	default:
		return false
	}
}

func resolveRolloutPath(root, sourceRoot, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "\x00") {
		return "", false
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", false
	}
	if filepath.IsAbs(filepath.FromSlash(value)) {
		absoluteValue := filepath.Clean(filepath.FromSlash(value))
		if relative, ok := relativeWithin(absoluteRoot, absoluteValue); ok {
			return relative, true
		}
		if strings.TrimSpace(sourceRoot) == "" {
			return "", false
		}
		source, err := filepath.Abs(sourceRoot)
		if err != nil {
			return "", false
		}
		if relative, ok := relativeWithin(source, absoluteValue); ok {
			return relative, true
		}
		return "", false
	}
	return safeRelativePath(value)
}

func relativeWithin(root, value string) (string, bool) {
	relative, err := filepath.Rel(root, value)
	if err != nil {
		return "", false
	}
	return safeRelativePath(filepath.ToSlash(relative))
}

func safeRelativePath(value string) (string, bool) {
	clean, err := realShapeSafeRelative(value)
	if err != nil || clean != filepath.ToSlash(filepath.Clean(filepath.FromSlash(value))) {
		return "", false
	}
	return clean, true
}

func realShapeSQLiteSemantics(ctx context.Context, root string) (string, error) {
	semantics := make(map[string][][]any, len(realShapeDatabases))
	for _, databaseName := range realShapeDatabases {
		path, err := realShapeReadOnlyPath(root, databaseName)
		if err != nil {
			return "", err
		}
		database, err := openReadOnlySQLite(ctx, path)
		if err != nil {
			return "", err
		}
		rows, err := database.QueryContext(ctx, "SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name")
		if err != nil {
			database.Close()
			return "", err
		}
		values := make([][]any, 0)
		for rows.Next() {
			row := make([]any, 4)
			pointers := make([]any, len(row))
			for index := range row {
				pointers[index] = &row[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				database.Close()
				return "", err
			}
			values = append(values, row)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			database.Close()
			return "", err
		}
		rows.Close()
		database.Close()
		semantics[databaseName] = values
	}
	encoded, err := json.Marshal(semantics)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func realShapeRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("真实结构 fixture 根必须是普通目录")
	}
	return absolute, nil
}

func realShapeReadOnlyPath(root, relative string) (string, error) {
	clean, err := realShapeSafeRelative(relative)
	if err != nil {
		return "", err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidate := absoluteRoot
	for _, part := range strings.Split(filepath.FromSlash(clean), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		candidate = filepath.Join(candidate, part)
		info, statErr := os.Lstat(candidate)
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("路径包含符号链接")
		}
	}
	info, err := os.Lstat(candidate)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("路径不是普通文件")
	}
	return candidate, nil
}

// realShapeSafeRelative 仅允许 fixture 根目录下的普通相对路径。
// 这里独立实现路径校验，避免兼容层依赖归档包的内部安全函数。
func realShapeSafeRelative(value string) (string, error) {
	if strings.TrimSpace(value) == "" || strings.Contains(value, "\x00") {
		return "", fmt.Errorf("fixture 相对路径为空或包含 NUL")
	}
	clean := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.HasPrefix(clean, "/") || filepath.VolumeName(filepath.FromSlash(clean)) != "" || (len(clean) >= 2 && clean[1] == ':') {
		return "", fmt.Errorf("fixture 路径越出根目录: %s", value)
	}
	return clean, nil
}

func realShapeHashFile(ctx context.Context, path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
