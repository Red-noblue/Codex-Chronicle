// 中文说明：为 synthetic-fixture-v1 提供 SQLite 行快照、事务删除和冲突恢复；真实 .codex 永远不会进入此适配器。
package compat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"
)

// SyntheticSQLiteMutation 只处理显式合成 fixture 的 SQLite 控制面，不识别真实 Codex 版本。
type SyntheticSQLiteMutation struct {
	descriptor        Descriptor
	snapshotDirectory string
}

// NewSyntheticSQLiteMutation 创建 fixture 快照目录；目录必须位于源根之外。
func NewSyntheticSQLiteMutation(snapshotDirectory string) (*SyntheticSQLiteMutation, error) {
	if strings.TrimSpace(snapshotDirectory) == "" {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_MISSING", Message: "fixture mutation 必须配置独立快照目录", ReadOnly: true}
	}
	abs, err := filepath.Abs(snapshotDirectory)
	if err != nil {
		return nil, fmt.Errorf("解析 fixture 快照目录失败: %w", err)
	}
	if pathHasCodexComponent(abs) {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_CODEX", Message: "fixture 快照目录不能位于 .codex 路径", ReadOnly: true}
	}
	if info, statErr := os.Lstat(abs); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_SYMLINK", Message: "fixture 快照目录不能是符号链接", ReadOnly: true}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("检查 fixture 快照目录失败: %w", statErr)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("创建 fixture 快照目录失败: %w", err)
	}
	return &SyntheticSQLiteMutation{descriptor: SyntheticFixtureDescriptor(), snapshotDirectory: filepath.Clean(abs)}, nil
}

// Descriptor 返回仅对合成 fixture 授权的能力描述。
func (m *SyntheticSQLiteMutation) Descriptor() Descriptor {
	if m == nil {
		return GenericJSONLDescriptor()
	}
	return m.descriptor
}

// Prepare 以只读连接收集所有指向 conversation 的 SQLite 行，并把行快照写入独立目录。
func (m *SyntheticSQLiteMutation) Prepare(ctx context.Context, request MutationRequest) (MutationSnapshot, error) {
	if err := m.validateAdapter(ctx, request); err != nil {
		return MutationSnapshot{}, err
	}
	rows := make([]sqliteRowSnapshot, 0)
	indexes := make([]sessionIndexSnapshot, 0)
	targets := mutationTargets(request)
	sawSQLite := false
	sawControlPlane := false
	for _, asset := range request.Assets {
		path, err := fixturePath(request.Root, asset.RelativePath)
		if err != nil {
			return MutationSnapshot{}, err
		}
		if err := verifyFixtureAsset(path, asset); err != nil {
			return MutationSnapshot{}, err
		}
		if asset.Kind == domain.AssetKindSessionIndex {
			sawControlPlane = true
			index, indexErr := collectSessionIndexSnapshot(ctx, path, asset.RelativePath, targets)
			if indexErr != nil {
				return MutationSnapshot{}, indexErr
			}
			indexes = append(indexes, index)
			continue
		}
		if asset.Kind != domain.AssetKindSQLite {
			continue
		}
		sawSQLite = true
		sawControlPlane = true
		database, err := openFixtureSQLite(ctx, path, true)
		if err != nil {
			return MutationSnapshot{}, err
		}
		fileRows, readErr := collectSQLiteRows(ctx, database, asset.RelativePath, targets)
		closeErr := database.Close()
		if readErr != nil {
			return MutationSnapshot{}, readErr
		}
		if closeErr != nil {
			return MutationSnapshot{}, fmt.Errorf("关闭 fixture SQLite 失败: %w", closeErr)
		}
		rows = append(rows, fileRows...)
	}
	if !sawSQLite && !sawControlPlane {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_SQLITE_ASSET_MISSING", Message: "fixture mutation 请求没有 SQLite 资产", ReadOnly: true}
	}
	if len(rows) == 0 && len(indexes) == 0 {
		return MutationSnapshot{}, &MutationError{Code: "MUTATION_RELATION_ROWS_MISSING", Message: "没有找到与目标对话关联的 SQLite 行，拒绝猜测删除范围", ReadOnly: true}
	}
	sortSQLiteRows(rows)
	payload := sqliteMutationPayload{
		Version: 1, AdapterID: m.descriptor.ID, Root: filepath.Clean(request.Root), ConversationID: request.ConversationID,
		SourceID: request.SourceID, AssetSetHash: request.AssetSetHash, Assets: append([]MutationAsset(nil), request.Assets...), Rows: rows, Indexes: indexes,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return MutationSnapshot{}, fmt.Errorf("序列化 SQLite 行快照失败: %w", err)
	}
	path, err := m.writeSnapshot(encoded)
	if err != nil {
		return MutationSnapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	request.SnapshotRef = path
	request.SnapshotSHA256 = hex.EncodeToString(digest[:])
	return BuildMutationSnapshot(ctx, m.descriptor.ID, request)
}

// Apply 在每个数据库内以 BEGIN IMMEDIATE + 行快照比较执行删除；跨库局部失败会返回 needs_review。
func (m *SyntheticSQLiteMutation) Apply(ctx context.Context, snapshot MutationSnapshot, action MutationAction) (MutationOutcome, error) {
	if action != MutationDelete {
		return MutationOutcome{}, unsupportedMutationAction(action)
	}
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return MutationOutcome{}, err
	}
	rowsByDatabase := groupSQLiteRows(payload.Rows)
	outcome := MutationOutcome{Action: action, AdapterID: snapshot.AdapterID, SnapshotHash: snapshot.SnapshotHash}
	for _, index := range payload.Indexes {
		changed, applyErr := applySessionIndex(ctx, snapshot.Root, index, false)
		if applyErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, applyErr
		}
		outcome.ChangedFiles += changed
	}
	for databasePath, rows := range rowsByDatabase {
		changed, applyErr := m.applyDatabase(ctx, snapshot.Root, databasePath, rows, false)
		if applyErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, applyErr
		}
		outcome.ChangedRows += changed
	}
	if err := m.Verify(ctx, snapshot, outcome); err != nil {
		outcome.NeedsReview = true
		return outcome, err
	}
	outcome.Verified = true
	outcome.Verification = []string{"SQLite 目标行和 session index 目标记录已重新读取并确认不存在"}
	return outcome, nil
}

// Verify 只读检查快照中的所有目标行均已消失，不修改数据库。
func (m *SyntheticSQLiteMutation) Verify(ctx context.Context, snapshot MutationSnapshot, outcome MutationOutcome) error {
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return err
	}
	rowsByDatabase := groupSQLiteRows(payload.Rows)
	for _, index := range payload.Indexes {
		if err := verifySessionIndex(ctx, snapshot.Root, index, false); err != nil {
			return err
		}
	}
	for databasePath, rows := range rowsByDatabase {
		path, pathErr := fixturePath(snapshot.Root, databasePath)
		if pathErr != nil {
			return pathErr
		}
		if regularErr := ensureRegularFixturePath(path); regularErr != nil {
			return regularErr
		}
		database, openErr := openFixtureSQLite(ctx, path, true)
		if openErr != nil {
			return openErr
		}
		for _, row := range rows {
			exists, checkErr := sqliteRowMatches(ctx, database, row)
			if checkErr != nil {
				_ = database.Close()
				return checkErr
			}
			if exists {
				_ = database.Close()
				return fmt.Errorf("SQLite 行仍存在：%s.%s", row.Database, row.Table)
			}
		}
		if closeErr := database.Close(); closeErr != nil {
			return closeErr
		}
	}
	return nil
}

// Restore 按快照恢复缺失行，遇到现有不同内容时拒绝覆盖并进入人工复核。
func (m *SyntheticSQLiteMutation) Restore(ctx context.Context, snapshot MutationSnapshot) (MutationOutcome, error) {
	payload, err := m.loadSnapshot(ctx, snapshot)
	if err != nil {
		return MutationOutcome{}, err
	}
	rowsByDatabase := groupSQLiteRows(payload.Rows)
	outcome := MutationOutcome{Action: MutationRestore, AdapterID: snapshot.AdapterID, SnapshotHash: snapshot.SnapshotHash}
	for _, index := range payload.Indexes {
		changed, restoreErr := applySessionIndex(ctx, snapshot.Root, index, true)
		if restoreErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, restoreErr
		}
		outcome.ChangedFiles += changed
		if verifyErr := verifySessionIndex(ctx, snapshot.Root, index, true); verifyErr != nil {
			outcome.NeedsReview = true
			return outcome, verifyErr
		}
	}
	for databasePath, rows := range rowsByDatabase {
		changed, restoreErr := m.applyDatabase(ctx, snapshot.Root, databasePath, rows, true)
		if restoreErr != nil {
			outcome.NeedsReview = outcome.ChangedRows > 0 || outcome.ChangedFiles > 0
			return outcome, restoreErr
		}
		outcome.ChangedRows += changed
	}
	outcome.Verified = true
	outcome.Verification = []string{"SQLite 行和 session index 已按快照恢复，并拒绝覆盖冲突对象"}
	return outcome, nil
}

func (m *SyntheticSQLiteMutation) validateAdapter(ctx context.Context, request MutationRequest) error {
	if m == nil {
		return &MutationError{Code: "MUTATION_ADAPTER_MISSING", Message: "fixture SQLite mutation adapter 为空", ReadOnly: true}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.descriptor.ID != SyntheticFixtureDescriptor().ID || !m.descriptor.AllowsWrite(CapabilityDelete) {
		return &MutationError{Code: "MUTATION_ADAPTER_NOT_AUTHORIZED", Message: "当前 adapter 未授权 synthetic fixture 写入", ReadOnly: true}
	}
	if pathHasCodexComponent(request.Root) {
		return &MutationError{Code: "MUTATION_FIXTURE_CODEX_ROOT", Message: "fixture mutation 拒绝 .codex 根目录", ReadOnly: true}
	}
	if err := ValidateMutationRequest(ctx, request); err != nil {
		return err
	}
	if pathsOverlap(request.Root, m.snapshotDirectory) {
		return &MutationError{Code: "MUTATION_SNAPSHOT_DIRECTORY_OVERLAP", Message: "fixture 快照目录不能与源根重叠", ReadOnly: true}
	}
	return nil
}

func (m *SyntheticSQLiteMutation) writeSnapshot(encoded []byte) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", fmt.Errorf("生成 fixture 快照 ID 失败: %w", err)
		}
		path := filepath.Join(m.snapshotDirectory, "mutation-"+hex.EncodeToString(random[:])+".json")
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", fmt.Errorf("创建 fixture 行快照失败: %w", err)
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("写入 fixture 行快照失败: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("同步 fixture 行快照失败: %w", err)
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("关闭 fixture 行快照失败: %w", err)
		}
		return path, nil
	}
	return "", fmt.Errorf("生成 fixture 行快照 ID 冲突")
}

func (m *SyntheticSQLiteMutation) loadSnapshot(ctx context.Context, snapshot MutationSnapshot) (sqliteMutationPayload, error) {
	if err := ValidateMutationSnapshot(ctx, snapshot); err != nil {
		return sqliteMutationPayload{}, err
	}
	if strings.TrimSpace(snapshot.SnapshotRef) == "" || !pathWithin(m.snapshotDirectory, snapshot.SnapshotRef) {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_REFERENCE_INVALID", Message: "fixture 行快照引用越出快照目录", ReadOnly: true}
	}
	info, err := os.Lstat(snapshot.SnapshotRef)
	if err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("读取 fixture 行快照失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_NOT_REGULAR", Message: "fixture 行快照不是普通文件", ReadOnly: true}
	}
	data, err := os.ReadFile(snapshot.SnapshotRef)
	if err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("读取 fixture 行快照失败: %w", err)
	}
	digest := sha256.Sum256(dataWithoutTrailingNewline(data))
	if hex.EncodeToString(digest[:]) != snapshot.SnapshotSHA256 {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_TAMPERED", Message: "fixture 行快照摘要不匹配", ReadOnly: true}
	}
	var payload sqliteMutationPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return sqliteMutationPayload{}, fmt.Errorf("解析 fixture 行快照失败: %w", err)
	}
	if payload.Version != 1 || payload.AdapterID != snapshot.AdapterID || filepath.Clean(payload.Root) != filepath.Clean(snapshot.Root) || payload.ConversationID != snapshot.ConversationID || payload.SourceID != snapshot.SourceID || (len(payload.Rows) == 0 && len(payload.Indexes) == 0) {
		return sqliteMutationPayload{}, &MutationError{Code: "MUTATION_SNAPSHOT_IDENTITY_INVALID", Message: "fixture 行快照身份不匹配", ReadOnly: true}
	}
	return payload, nil
}

func (m *SyntheticSQLiteMutation) applyDatabase(ctx context.Context, root, relativeDatabase string, rows []sqliteRowSnapshot, restore bool) (int, error) {
	path, err := fixturePath(root, relativeDatabase)
	if err != nil {
		return 0, err
	}
	if err := ensureRegularFixturePath(path); err != nil {
		return 0, err
	}
	database, err := openFixtureSQLite(ctx, path, false)
	if err != nil {
		return 0, err
	}
	database.SetMaxOpenConns(1)
	defer database.Close()
	if _, err := database.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("锁定 fixture SQLite 失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = database.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	changed := 0
	for _, row := range rows {
		current, exists, readErr := readSQLiteRow(ctx, database, row)
		if readErr != nil {
			return changed, readErr
		}
		if restore {
			if exists {
				if !sqlValuesEqual(current, row.Values) {
					return changed, fmt.Errorf("恢复冲突：%s.%s 的现有行已变化", row.Database, row.Table)
				}
				continue
			}
			if err := insertSQLiteRow(ctx, database, row); err != nil {
				return changed, err
			}
			changed++
			continue
		}
		if !exists {
			return changed, fmt.Errorf("删除前 SQLite 行已不存在：%s.%s", row.Database, row.Table)
		}
		if !sqlValuesEqual(current, row.Values) {
			return changed, fmt.Errorf("删除前 SQLite 行已变化：%s.%s", row.Database, row.Table)
		}
		if err := deleteSQLiteRow(ctx, database, row); err != nil {
			return changed, err
		}
		changed++
	}
	if _, err := database.ExecContext(ctx, "COMMIT"); err != nil {
		return changed, fmt.Errorf("提交 fixture SQLite 事务失败: %w", err)
	}
	committed = true
	return changed, nil
}

type sqliteMutationPayload struct {
	Version        int                    `json:"version"`
	AdapterID      string                 `json:"adapter_id"`
	Root           string                 `json:"root"`
	ConversationID string                 `json:"conversation_id"`
	SourceID       string                 `json:"source_id,omitempty"`
	AssetSetHash   string                 `json:"asset_set_hash,omitempty"`
	Assets         []MutationAsset        `json:"assets"`
	Rows           []sqliteRowSnapshot    `json:"rows"`
	Indexes        []sessionIndexSnapshot `json:"session_indexes,omitempty"`
}

// sessionIndexSnapshot 保存一个 session index 在删除前后的完整字节版本；不把整份 index 当作普通文件移动。
type sessionIndexSnapshot struct {
	RelativePath   string   `json:"relative_path"`
	OriginalSHA256 string   `json:"original_sha256"`
	OriginalBytes  string   `json:"original_bytes"`
	AfterSHA256    string   `json:"after_sha256"`
	AfterBytes     string   `json:"after_bytes"`
	RemovedLines   []uint64 `json:"removed_lines"`
}

type sqliteRowSnapshot struct {
	Database       string        `json:"database"`
	Table          string        `json:"table"`
	Columns        []string      `json:"columns"`
	PrimaryColumns []string      `json:"primary_columns"`
	Values         []sqliteValue `json:"values"`
}

type sqliteValue struct {
	Kind  string  `json:"kind"`
	Text  string  `json:"text,omitempty"`
	Int   int64   `json:"int,omitempty"`
	Float float64 `json:"float,omitempty"`
	Bytes string  `json:"bytes,omitempty"`
}

type sqliteSchema struct {
	Columns        []string
	PrimaryColumns []string
}

func mutationTargets(request MutationRequest) []string {
	values := []string{request.SourceID, request.ConversationID}
	seen := make(map[string]struct{}, len(values))
	targets := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		targets = append(targets, value)
	}
	return targets
}

func collectSessionIndexSnapshot(ctx context.Context, path, relative string, targets []string) (sessionIndexSnapshot, error) {
	original, err := os.ReadFile(path)
	if err != nil {
		return sessionIndexSnapshot{}, fmt.Errorf("读取 fixture session index 失败: %w", err)
	}
	kept := make([]byte, 0, len(original))
	removed := make([]uint64, 0)
	_, scanErr := catalog.ScanReaderContent(bytes.NewReader(original), func(frame domain.JSONLFrame, content []byte) error {
		if !frame.ValidJSON || frame.Pending {
			return fmt.Errorf("session index 第 %d 行不是稳定 JSON，拒绝写入", frame.LineNumber)
		}
		start, end := frame.StartOffset, frame.EndOffset
		if start < 0 || end < start || end > int64(len(original)) {
			return fmt.Errorf("session index 第 %d 行范围无效", frame.LineNumber)
		}
		if sessionIndexMatches(content, targets) {
			removed = append(removed, frame.LineNumber)
			return nil
		}
		kept = append(kept, original[start:end]...)
		return nil
	})
	if scanErr != nil {
		return sessionIndexSnapshot{}, scanErr
	}
	if len(removed) == 0 {
		return sessionIndexSnapshot{}, &MutationError{Code: "MUTATION_INDEX_RELATION_MISSING", Message: fmt.Sprintf("session index %s 没有目标对话记录，拒绝猜测重写", relative), ReadOnly: true}
	}
	originalHash := hashBytes(original)
	afterHash := hashBytes(kept)
	return sessionIndexSnapshot{
		RelativePath: relative, OriginalSHA256: originalHash, OriginalBytes: base64.StdEncoding.EncodeToString(original),
		AfterSHA256: afterHash, AfterBytes: base64.StdEncoding.EncodeToString(kept), RemovedLines: removed,
	}, nil
}

func sessionIndexMatches(content []byte, targets []string) bool {
	var record map[string]json.RawMessage
	if err := json.Unmarshal(content, &record); err != nil {
		return false
	}
	for _, key := range []string{"id", "thread_id", "session_id", "conversation_id"} {
		value := rawString(record[key])
		for _, target := range targets {
			if value != "" && value == target {
				return true
			}
		}
	}
	return false
}

func verifySessionIndex(ctx context.Context, root string, snapshot sessionIndexSnapshot, restored bool) error {
	path, err := fixturePath(root, snapshot.RelativePath)
	if err != nil {
		return err
	}
	if err := ensureRegularFixturePath(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取 fixture session index 失败: %w", err)
	}
	want := snapshot.AfterSHA256
	if restored {
		want = snapshot.OriginalSHA256
	}
	if hashBytes(data) != want {
		return fmt.Errorf("session index 状态未达到预期：%s", snapshot.RelativePath)
	}
	return nil
}

func applySessionIndex(ctx context.Context, root string, snapshot sessionIndexSnapshot, restore bool) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path, err := fixturePath(root, snapshot.RelativePath)
	if err != nil {
		return 0, err
	}
	if err := ensureRegularFixturePath(path); err != nil {
		return 0, err
	}
	original, err := base64.StdEncoding.DecodeString(snapshot.OriginalBytes)
	if err != nil || hashBytes(original) != snapshot.OriginalSHA256 {
		return 0, &MutationError{Code: "MUTATION_INDEX_SNAPSHOT_INVALID", Message: "session index 原始快照摘要无效", ReadOnly: true}
	}
	after, err := base64.StdEncoding.DecodeString(snapshot.AfterBytes)
	if err != nil || hashBytes(after) != snapshot.AfterSHA256 {
		return 0, &MutationError{Code: "MUTATION_INDEX_SNAPSHOT_INVALID", Message: "session index 删除快照摘要无效", ReadOnly: true}
	}
	current, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("读取 fixture session index 失败: %w", err)
	}
	currentHash := hashBytes(current)
	if restore {
		if currentHash == snapshot.OriginalSHA256 {
			return 0, nil
		}
		if currentHash != snapshot.AfterSHA256 {
			return 0, fmt.Errorf("恢复 session index 冲突：%s", snapshot.RelativePath)
		}
		if err := writeSessionIndexAtomically(path, original); err != nil {
			return 0, err
		}
		return 1, nil
	}
	if currentHash == snapshot.AfterSHA256 {
		return 0, nil
	}
	if currentHash != snapshot.OriginalSHA256 {
		return 0, fmt.Errorf("删除前 session index 已变化：%s", snapshot.RelativePath)
	}
	if err := writeSessionIndexAtomically(path, after); err != nil {
		return 0, err
	}
	return 1, nil
}

func writeSessionIndexAtomically(path string, data []byte) error {
	temporary := path + ".mutation.tmp"
	if _, err := os.Lstat(temporary); err == nil {
		return fmt.Errorf("session index 临时目标已存在：%s", temporary)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("创建 session index 临时文件失败: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("写入 session index 临时文件失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("同步 session index 临时文件失败: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("关闭 session index 临时文件失败: %w", err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("发布 session index 失败: %w", err)
	}
	cleanup = false
	return nil
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func collectSQLiteRows(ctx context.Context, database *sql.DB, relativeDatabase string, targets []string) ([]sqliteRowSnapshot, error) {
	tables, err := sqliteTableNames(ctx, database)
	if err != nil {
		return nil, err
	}
	rows := make([]sqliteRowSnapshot, 0)
	seen := make(map[string]struct{})
	for _, table := range tables {
		schema, schemaErr := readSQLiteSchema(ctx, database, table)
		if schemaErr != nil {
			return nil, schemaErr
		}
		for _, column := range relationColumns(table, schema.Columns) {
			for _, target := range targets {
				query := `SELECT * FROM "` + quoteIdentifier(table) + `" WHERE "` + quoteIdentifier(column) + `" = ?`
				result, queryErr := database.QueryContext(ctx, query, target)
				if queryErr != nil {
					return nil, fmt.Errorf("读取 fixture 关系 %s.%s 失败: %w", table, column, queryErr)
				}
				columns, columnErr := result.Columns()
				if columnErr != nil {
					_ = result.Close()
					return nil, fmt.Errorf("读取 fixture 表 %s 列失败: %w", table, columnErr)
				}
				for result.Next() {
					values, scanErr := scanSQLiteValues(result, len(columns))
					if scanErr != nil {
						_ = result.Close()
						return nil, fmt.Errorf("读取 fixture 行 %s 失败: %w", table, scanErr)
					}
					if len(schema.PrimaryColumns) == 0 {
						_ = result.Close()
						return nil, fmt.Errorf("fixture 关系表 %s 没有稳定主键，拒绝写入", table)
					}
					row := sqliteRowSnapshot{Database: filepath.ToSlash(relativeDatabase), Table: table, Columns: append([]string(nil), columns...), PrimaryColumns: append([]string(nil), schema.PrimaryColumns...), Values: values}
					key, keyErr := sqliteRowKey(row)
					if keyErr != nil {
						_ = result.Close()
						return nil, keyErr
					}
					if _, exists := seen[key]; !exists {
						seen[key] = struct{}{}
						rows = append(rows, row)
					}
				}
				if err := result.Err(); err != nil {
					_ = result.Close()
					return nil, fmt.Errorf("遍历 fixture 关系 %s.%s 失败: %w", table, column, err)
				}
				if err := result.Close(); err != nil {
					return nil, err
				}
			}
		}
	}
	return rows, nil
}

func sqliteTableNames(ctx context.Context, database *sql.DB) ([]string, error) {
	rows, err := database.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("读取 fixture 表清单失败: %w", err)
	}
	defer rows.Close()
	names := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

func readSQLiteSchema(ctx context.Context, database *sql.DB, table string) (sqliteSchema, error) {
	rows, err := database.QueryContext(ctx, `PRAGMA table_info("`+quoteIdentifier(table)+`")`)
	if err != nil {
		return sqliteSchema{}, fmt.Errorf("读取 fixture 表 %s schema 失败: %w", table, err)
	}
	defer rows.Close()
	type columnInfo struct {
		cid  int
		name string
		pk   int
	}
	columns := make([]columnInfo, 0)
	for rows.Next() {
		var item columnInfo
		var columnType string
		var notNull int
		var defaultValue any
		if err := rows.Scan(&item.cid, &item.name, &columnType, &notNull, &defaultValue, &item.pk); err != nil {
			return sqliteSchema{}, err
		}
		columns = append(columns, item)
	}
	if err := rows.Err(); err != nil {
		return sqliteSchema{}, err
	}
	schema := sqliteSchema{Columns: make([]string, 0, len(columns)), PrimaryColumns: make([]string, 0)}
	for _, column := range columns {
		schema.Columns = append(schema.Columns, column.name)
	}
	sort.Slice(columns, func(i, j int) bool { return columns[i].pk < columns[j].pk })
	for _, column := range columns {
		if column.pk > 0 {
			schema.PrimaryColumns = append(schema.PrimaryColumns, column.name)
		}
	}
	return schema, nil
}

func relationColumns(table string, columns []string) []string {
	result := make([]string, 0)
	for _, column := range columns {
		if column == "id" && table == "threads" {
			result = append(result, column)
			continue
		}
		switch column {
		case "thread_id", "parent_thread_id", "child_thread_id", "session_id", "conversation_id":
			result = append(result, column)
		}
	}
	return result
}

func scanSQLiteValues(rows *sql.Rows, count int) ([]sqliteValue, error) {
	raw := make([]any, count)
	pointers := make([]any, count)
	for index := range raw {
		pointers[index] = &raw[index]
	}
	if err := rows.Scan(pointers...); err != nil {
		return nil, err
	}
	values := make([]sqliteValue, count)
	for index, value := range raw {
		values[index] = encodeSQLiteValue(value)
	}
	return values, nil
}

func encodeSQLiteValue(value any) sqliteValue {
	switch typed := value.(type) {
	case nil:
		return sqliteValue{Kind: "null"}
	case int64:
		return sqliteValue{Kind: "int", Int: typed}
	case int:
		return sqliteValue{Kind: "int", Int: int64(typed)}
	case float64:
		return sqliteValue{Kind: "float", Float: typed}
	case []byte:
		return sqliteValue{Kind: "bytes", Bytes: base64.StdEncoding.EncodeToString(typed)}
	case string:
		return sqliteValue{Kind: "text", Text: typed}
	default:
		return sqliteValue{Kind: "text", Text: fmt.Sprint(typed)}
	}
}

func decodeSQLiteValue(value sqliteValue) (any, error) {
	switch value.Kind {
	case "null":
		return nil, nil
	case "int":
		return value.Int, nil
	case "float":
		return value.Float, nil
	case "text":
		return value.Text, nil
	case "bytes":
		decoded, err := base64.StdEncoding.DecodeString(value.Bytes)
		if err != nil {
			return nil, fmt.Errorf("SQLite 二进制值无效: %w", err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("SQLite 值类型不受支持: %s", value.Kind)
	}
}

func sqliteRowKey(row sqliteRowSnapshot) (string, error) {
	values := make([]sqliteValue, 0, len(row.PrimaryColumns))
	for _, column := range row.PrimaryColumns {
		index := indexOf(row.Columns, column)
		if index < 0 || index >= len(row.Values) {
			return "", fmt.Errorf("fixture 行主键列不存在: %s.%s", row.Table, column)
		}
		values = append(values, row.Values[index])
	}
	encoded, err := json.Marshal(struct {
		Database string        `json:"database"`
		Table    string        `json:"table"`
		Columns  []string      `json:"columns"`
		Values   []sqliteValue `json:"values"`
	}{row.Database, row.Table, row.PrimaryColumns, values})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func groupSQLiteRows(rows []sqliteRowSnapshot) map[string][]sqliteRowSnapshot {
	groups := make(map[string][]sqliteRowSnapshot)
	for _, row := range rows {
		groups[row.Database] = append(groups[row.Database], row)
	}
	for database := range groups {
		sort.Slice(groups[database], func(i, j int) bool {
			left, _ := sqliteRowKey(groups[database][i])
			right, _ := sqliteRowKey(groups[database][j])
			return left < right
		})
	}
	return groups
}

func sortSQLiteRows(rows []sqliteRowSnapshot) {
	sort.Slice(rows, func(i, j int) bool {
		left, _ := sqliteRowKey(rows[i])
		right, _ := sqliteRowKey(rows[j])
		return left < right
	})
}

func readSQLiteRow(ctx context.Context, database *sql.DB, row sqliteRowSnapshot) ([]sqliteValue, bool, error) {
	where, args, err := primaryWhere(row)
	if err != nil {
		return nil, false, err
	}
	query := `SELECT "` + joinQuoted(row.Columns) + `" FROM "` + quoteIdentifier(row.Table) + `" WHERE ` + where
	result, err := database.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("读取 fixture 行失败 %s.%s: %w", row.Database, row.Table, err)
	}
	defer result.Close()
	if !result.Next() {
		if err := result.Err(); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}
	values, err := scanSQLiteValues(result, len(row.Columns))
	if err != nil {
		return nil, false, err
	}
	return values, true, nil
}

func sqliteRowMatches(ctx context.Context, database *sql.DB, row sqliteRowSnapshot) (bool, error) {
	values, exists, err := readSQLiteRow(ctx, database, row)
	if err != nil || !exists {
		return exists, err
	}
	return sqlValuesEqual(values, row.Values), nil
}

func deleteSQLiteRow(ctx context.Context, database *sql.DB, row sqliteRowSnapshot) error {
	where, args, err := primaryWhere(row)
	if err != nil {
		return err
	}
	result, err := database.ExecContext(ctx, `DELETE FROM "`+quoteIdentifier(row.Table)+`" WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("删除 fixture 行失败 %s.%s: %w", row.Database, row.Table, err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("删除 fixture 行数量异常 %s.%s: %d", row.Database, row.Table, affected)
	}
	return nil
}

func insertSQLiteRow(ctx context.Context, database *sql.DB, row sqliteRowSnapshot) error {
	if len(row.Columns) != len(row.Values) {
		return fmt.Errorf("恢复 fixture 行列和值数量不一致: %s.%s", row.Database, row.Table)
	}
	values := make([]any, len(row.Values))
	for index, value := range row.Values {
		decoded, err := decodeSQLiteValue(value)
		if err != nil {
			return err
		}
		values[index] = decoded
	}
	placeholders := make([]string, len(row.Columns))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	query := `INSERT INTO "` + quoteIdentifier(row.Table) + `" ("` + joinQuoted(row.Columns) + `") VALUES (` + strings.Join(placeholders, ",") + ")"
	if _, err := database.ExecContext(ctx, query, values...); err != nil {
		return fmt.Errorf("恢复 fixture 行失败 %s.%s: %w", row.Database, row.Table, err)
	}
	return nil
}

func primaryWhere(row sqliteRowSnapshot) (string, []any, error) {
	parts := make([]string, 0, len(row.PrimaryColumns))
	args := make([]any, 0, len(row.PrimaryColumns))
	for _, column := range row.PrimaryColumns {
		index := indexOf(row.Columns, column)
		if index < 0 || index >= len(row.Values) {
			return "", nil, fmt.Errorf("fixture 行缺少主键值: %s.%s", row.Table, column)
		}
		value, err := decodeSQLiteValue(row.Values[index])
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, `"`+quoteIdentifier(column)+`" = ?`)
		args = append(args, value)
	}
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("fixture 行没有主键条件: %s.%s", row.Database, row.Table)
	}
	return strings.Join(parts, " AND "), args, nil
}

func sqlValuesEqual(left, right []sqliteValue) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind || left[index].Text != right[index].Text || left[index].Int != right[index].Int || left[index].Float != right[index].Float || left[index].Bytes != right[index].Bytes {
			return false
		}
	}
	return true
}

func openFixtureSQLite(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(abs) + "?_pragma=busy_timeout(1000)"
	if readOnly {
		readOnlyDSN, _, dsnErr := sqliteguard.ReadOnlyDSN(abs, 1000)
		if dsnErr != nil {
			return nil, dsnErr
		}
		dsn = readOnlyDSN
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 fixture SQLite 失败: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("连接 fixture SQLite 失败: %w", err)
	}
	return database, nil
}

func verifyFixtureAsset(path string, asset MutationAsset) error {
	if err := ensureRegularFixturePath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取 fixture 资产失败: %w", err)
	}
	if info.Size() != asset.SizeBytes {
		return fmt.Errorf("fixture 资产大小已变化：%s", asset.RelativePath)
	}
	digest, err := hashFixtureFile(path)
	if err != nil {
		return err
	}
	if digest != asset.SHA256 {
		return fmt.Errorf("fixture 资产摘要已变化：%s", asset.RelativePath)
	}
	return nil
}

func ensureRegularFixturePath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("读取 fixture 资产失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return &MutationError{Code: "MUTATION_ASSET_NOT_REGULAR", Message: "fixture 资产不是普通文件", ReadOnly: true}
	}
	return nil
}

func hashFixtureFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func fixturePath(root, relative string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &MutationError{Code: "MUTATION_ASSET_PATH_INVALID", Message: fmt.Sprintf("fixture 资产路径不安全：%s", relative), ReadOnly: true}
	}
	path := filepath.Join(absRoot, clean)
	if !pathWithin(absRoot, path) {
		return "", &MutationError{Code: "MUTATION_ASSET_PATH_INVALID", Message: fmt.Sprintf("fixture 资产越出源根：%s", relative), ReadOnly: true}
	}
	if err := ensureNoSymlinkComponents(absRoot, path); err != nil {
		return "", err
	}
	return path, nil
}

func ensureNoSymlinkComponents(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return &MutationError{Code: "MUTATION_ASSET_PATH_INVALID", Message: "fixture 路径越出源根", ReadOnly: true}
	}
	current := root
	for _, part := range strings.Split(filepath.Dir(relative), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return &MutationError{Code: "MUTATION_ASSET_PATH_SYMLINK", Message: "fixture 资产路径包含符号链接", ReadOnly: true}
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
	}
	return nil
}

func joinQuoted(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quoteIdentifier(value)
	}
	return strings.Join(quoted, `", "`)
}

func indexOf(values []string, wanted string) int {
	for index, value := range values {
		if value == wanted {
			return index
		}
	}
	return -1
}

func dataWithoutTrailingNewline(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		return data[:len(data)-1]
	}
	return data
}

func unsupportedMutationAction(action MutationAction) error {
	return &MutationError{Code: "MUTATION_ACTION_UNSUPPORTED", Capability: CapabilityDelete, Message: fmt.Sprintf("synthetic SQLite adapter 不支持动作：%s", action), ReadOnly: true}
}

func pathHasCodexComponent(path string) bool {
	for _, part := range strings.FieldsFunc(filepath.Clean(path), func(r rune) bool { return r == '/' || r == '\\' }) {
		if strings.EqualFold(part, ".codex") {
			return true
		}
	}
	return false
}

func pathsOverlap(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return true
	}
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != "." && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
