// 中文说明：管理本项目独立 archive.db 的迁移和只读 inventory 派生数据写入。
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
)

const (
	initialSchemaVersion = 1
	currentSchemaVersion = 9
)

// MigrationPreflight 是 archive.db 的只读迁移预检结果；它不会执行迁移或创建旁路文件。
type MigrationPreflight struct {
	Path              string   `json:"path"`
	CurrentVersion    int      `json:"current_version"`
	SupportedVersion  int      `json:"supported_version"`
	Status            string   `json:"status"`
	RequiresMigration bool     `json:"requires_migration"`
	FutureVersion     bool     `json:"future_version"`
	ReadOnly          bool     `json:"read_only"`
	AuxiliaryFiles    []string `json:"auxiliary_files,omitempty"`
}

// Store 是本项目独立控制面的数据库句柄，不代表 Codex 原始 SQLite。
type Store struct {
	db              *sql.DB
	path            string
	migrationBackup *MigrationBackup
	index           *searchIndex
}

// SyncResult 是一次 inventory 派生同步的技术结果。
type SyncResult struct {
	WorkspaceID           string `json:"workspace_id"`
	RunID                 string `json:"run_id"`
	FileCount             int    `json:"file_count"`
	ConversationCount     int    `json:"conversation_count"`
	ErrorCount            int    `json:"error_count"`
	ProjectionReused      int    `json:"projection_reused"`
	ProjectionRebuilt     int    `json:"projection_rebuilt"`
	ProjectionIncremental int    `json:"projection_incremental"`
	FramesAdded           int    `json:"frames_added"`
	FramesReused          int    `json:"frames_reused"`
}

// Open 打开并迁移 archive.db。路径位于 .codex 目录下时会拒绝，避免误写源数据。
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithOptions(ctx, path, OpenOptions{})
}

// OpenWithOptions 打开并迁移 archive.db；旧 schema 会先保存独立 SQLite 快照。
func OpenWithOptions(ctx context.Context, path string, options OpenOptions) (*Store, error) {
	dsn, err := prepareDSN(path)
	if err != nil {
		return nil, err
	}
	existing := false
	if path != ":memory:" {
		abs, absErr := filepath.Abs(filepath.Clean(path))
		if absErr != nil {
			return nil, fmt.Errorf("解析 archive.db 路径失败: %w", absErr)
		}
		info, statErr := os.Lstat(abs)
		switch {
		case statErr == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("archive.db 必须是普通文件，不能是链接或特殊文件")
			}
			existing = true
		case os.IsNotExist(statErr):
		default:
			return nil, fmt.Errorf("读取 archive.db 状态失败: %w", statErr)
		}
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 archive.db 失败: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	storedPath := path
	if path != ":memory:" {
		storedPath, _ = filepath.Abs(path)
	}
	store := &Store{db: database, path: filepath.Clean(storedPath)}
	resetSearchIndex := false
	if existing {
		version, known, versionErr := readMigrationVersion(ctx, database)
		if versionErr != nil {
			_ = database.Close()
			return nil, versionErr
		}
		if known && version < currentSchemaVersion {
			resetSearchIndex = true
		}
		if resetSearchIndex && !options.DisableMigrationBackup {
			backupRoot := strings.TrimSpace(options.MigrationBackupRoot)
			if backupRoot == "" {
				backupRoot = DefaultMigrationBackupRoot(store.path)
			}
			backup, backupErr := createMigrationBackup(ctx, database, store.path, backupRoot, version, currentSchemaVersion)
			if backupErr != nil {
				_ = database.Close()
				return nil, backupErr
			}
			store.migrationBackup = &backup
		}
	}
	if err := store.configure(ctx, path); err != nil {
		_ = database.Close()
		return nil, err
	}
	if err := store.migrate(ctx); err != nil {
		closeErr := database.Close()
		if store.migrationBackup != nil {
			restoreErr := RestoreMigrationBackup(ctx, store.path, store.migrationBackup.Path)
			if closeErr != nil && restoreErr != nil {
				return nil, fmt.Errorf("archive.db 迁移失败，关闭连接和恢复迁移前备份均失败（备份 %s）：close=%v restore=%v: %w", store.migrationBackup.Path, closeErr, restoreErr, err)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("archive.db 迁移失败，已恢复迁移前备份但关闭连接报告错误（备份 %s）：close=%v: %w", store.migrationBackup.Path, closeErr, err)
			}
			if restoreErr != nil {
				return nil, fmt.Errorf("archive.db 迁移失败，恢复迁移前备份失败（备份 %s）：%v: %w", store.migrationBackup.Path, restoreErr, err)
			}
			return nil, fmt.Errorf("archive.db 迁移失败，已自动恢复迁移前备份（备份 %s）：%w", store.migrationBackup.Path, err)
		}
		return nil, err
	}
	if err := store.openSearchIndex(ctx); err != nil {
		_ = database.Close()
		return nil, fmt.Errorf("打开独立搜索索引失败: %w", err)
	}
	if resetSearchIndex {
		// 旧控制面迁移会重建 frame/search 投影；独立加速库不能继续使用旧游标。
		if err := store.resetSearchIndex(ctx); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("重置旧搜索索引失败: %w", err)
		}
	}
	return store, nil
}

// MigrationBackup 返回本次打开时创建的迁移前快照；没有迁移时返回 false。
func (s *Store) MigrationBackup() (MigrationBackup, bool) {
	if s == nil || s.migrationBackup == nil {
		return MigrationBackup{}, false
	}
	return *s.migrationBackup, true
}

func readMigrationVersion(ctx context.Context, database *sql.DB) (int, bool, error) {
	var hasMigrationTable int
	if err := database.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&hasMigrationTable); err != nil {
		return 0, false, fmt.Errorf("读取 archive.db 迁移表状态失败: %w", err)
	}
	if hasMigrationTable == 0 {
		return 0, true, nil
	}
	var version int
	if err := database.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0, false, fmt.Errorf("读取 archive.db 迁移版本失败: %w", err)
	}
	return version, true, nil
}

// Path 返回 archive.db 的绝对路径，供工作区注册表记录独立控制面位置。
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Close 关闭控制面连接。
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if s.index != nil {
		s.index.mu.Lock()
		defer s.index.mu.Unlock()
		_ = s.index.db.Close()
		s.index.encoder.Close()
		s.index.decoder.Close()
	}
	return s.db.Close()
}

// SchemaVersion 返回当前已应用迁移版本。
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// SupportedSchemaVersion 返回当前程序支持的 archive.db 迁移版本。
func SupportedSchemaVersion() int {
	return currentSchemaVersion
}

// MigrationPreflight 对已打开的控制面返回只读迁移状态；不会触发 store.Open 的迁移逻辑。
func (s *Store) MigrationPreflight(ctx context.Context) (MigrationPreflight, error) {
	if s == nil || s.db == nil {
		return MigrationPreflight{}, fmt.Errorf("archive.db 未初始化")
	}
	if s.path != ":memory:" {
		return PreflightPath(ctx, s.path)
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return MigrationPreflight{}, err
	}
	return migrationPreflight(s.path, version, nil), nil
}

// OpenedMigrationPreflight 通过本项目已经打开的连接读取已提交 schema，包括 SQLite 正常维护的 WAL。
// 外部未知数据库仍须使用 PreflightPath，不能将此方法作为忽略源数据库 WAL 的入口。
func (s *Store) OpenedMigrationPreflight(ctx context.Context) (MigrationPreflight, error) {
	if s == nil || s.db == nil {
		return MigrationPreflight{}, fmt.Errorf("项目数据库尚未打开")
	}
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return MigrationPreflight{}, err
	}
	return migrationPreflight(s.path, version, nil), nil
}

// PreflightPath 以 immutable 只读连接读取现有 archive.db 的迁移版本。
// 数据库不存在、未来版本或缺少迁移表时只返回状态；函数不会创建文件、WAL 或 SHM。
func PreflightPath(ctx context.Context, path string) (MigrationPreflight, error) {
	if strings.TrimSpace(path) == "" || path == ":memory:" {
		return MigrationPreflight{}, fmt.Errorf("只读迁移预检需要现有 archive.db 文件路径")
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return MigrationPreflight{}, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return MigrationPreflight{}, fmt.Errorf("读取 archive.db 失败: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return MigrationPreflight{}, fmt.Errorf("archive.db 必须是普通文件，不能是链接或特殊文件")
	}
	dsn, auxiliary, err := sqliteguard.ReadOnlyDSN(abs, 1000)
	if err != nil {
		return MigrationPreflight{}, err
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return MigrationPreflight{}, fmt.Errorf("打开 archive.db 只读连接失败: %w", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	defer database.Close()
	if err := database.PingContext(ctx); err != nil {
		return MigrationPreflight{}, fmt.Errorf("连接 archive.db 只读视图失败: %w", err)
	}
	var hasMigrationTable int
	if err := database.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&hasMigrationTable); err != nil {
		return MigrationPreflight{}, fmt.Errorf("读取 archive.db 迁移表状态失败: %w", err)
	}
	version := 0
	if hasMigrationTable != 0 {
		if err := database.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
			return MigrationPreflight{}, fmt.Errorf("读取 archive.db 迁移版本失败: %w", err)
		}
	}
	return migrationPreflight(abs, version, auxiliary), nil
}

func migrationPreflight(path string, version int, auxiliary []string) MigrationPreflight {
	status := "ready"
	requiresMigration := false
	futureVersion := false
	if version < currentSchemaVersion {
		status = "migration_required"
		requiresMigration = true
	}
	if version > currentSchemaVersion {
		status = "future_schema"
		futureVersion = true
		requiresMigration = false
	}
	if len(auxiliary) > 0 {
		status = "auxiliary_present"
		requiresMigration = false
		futureVersion = false
	}
	return MigrationPreflight{Path: path, CurrentVersion: version, SupportedVersion: currentSchemaVersion, Status: status, RequiresMigration: requiresMigration, FutureVersion: futureVersion, ReadOnly: true, AuxiliaryFiles: append([]string(nil), auxiliary...)}
}

// AssetCount 返回控制面当前保存的文件资产数量，供健康检查和测试使用。
func (s *Store) AssetCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM assets").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// SyncInventory 将只读扫描结果写入独立控制面，不修改报告中的源文件。
func (s *Store) SyncInventory(ctx context.Context, report domain.InventoryReport) (SyncResult, error) {
	return s.syncInventory(ctx, report, true)
}

// SyncInventoryWithoutSearch 更新目录与阅读定位，搜索由独立后台任务处理。
func (s *Store) SyncInventoryWithoutSearch(ctx context.Context, report domain.InventoryReport) (SyncResult, error) {
	return s.syncInventory(ctx, report, false)
}

func (s *Store) syncInventory(ctx context.Context, report domain.InventoryReport, allowSearch bool) (SyncResult, error) {
	if !report.ReadOnly {
		return SyncResult{}, fmt.Errorf("拒绝同步非只读 inventory")
	}
	workspaceID := stableID("workspace", report.Root)
	runID, err := randomID("scan")
	if err != nil {
		return SyncResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SyncResult{}, fmt.Errorf("开始 inventory 事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces(id, root_path, adapter_id, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(root_path) DO UPDATE SET adapter_id=excluded.adapter_id, updated_at=excluded.updated_at`,
		workspaceID, report.Root, "generic-jsonl-readonly", now, now); err != nil {
		return SyncResult{}, fmt.Errorf("写入 workspace 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO scan_runs(id, workspace_id, started_at, completed_at, read_only, file_count, error_count)
		VALUES(?, ?, ?, ?, 1, ?, ?)`, runID, workspaceID, report.GeneratedAt.UTC().Format(time.RFC3339Nano), now, len(report.Files), len(report.Errors)); err != nil {
		return SyncResult{}, fmt.Errorf("写入 scan run 失败: %w", err)
	}

	currentPaths := make(map[string]struct{}, len(report.Files))
	projectionReused := 0
	projectionRebuilt := 0
	projectionIncremental := 0
	framesAdded := 0
	framesReused := 0
	for _, file := range report.Files {
		currentPaths[file.RelativePath] = struct{}{}
		assetID := stableID("asset", workspaceID+"\x00"+file.RelativePath)
		changed := boolInt(file.ChangedDuringScan)
		contentSHA256 := ""
		if file.JSONL != nil {
			contentSHA256 = file.JSONL.ContentSHA256
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets(id, workspace_id, relative_path, kind, size_bytes, mod_time_ns, status, error, changed_during_scan, content_sha256, first_seen_run_id, last_seen_run_id)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(workspace_id, relative_path) DO UPDATE SET
				kind=excluded.kind, size_bytes=excluded.size_bytes, mod_time_ns=excluded.mod_time_ns,
				status=excluded.status, error=excluded.error, changed_during_scan=excluded.changed_during_scan,
				content_sha256=excluded.content_sha256,
				last_seen_run_id=excluded.last_seen_run_id`,
			assetID, workspaceID, file.RelativePath, file.Kind, file.SizeBytes, file.ModTime.UnixNano(), file.Status, nullableString(file.Error), changed, contentSHA256, runID, runID); err != nil {
			return SyncResult{}, fmt.Errorf("写入 asset %s 失败: %w", file.RelativePath, err)
		}
		if file.JSONL != nil {
			typesJSON, marshalErr := json.Marshal(file.JSONL.TopLevelTypes)
			if marshalErr != nil {
				return SyncResult{}, fmt.Errorf("序列化 JSONL 类型统计失败: %w", marshalErr)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jsonl_summaries(asset_id, line_count, invalid_json_lines, invalid_utf8_lines, pending_lines, max_line_bytes, top_level_types_json)
				VALUES(?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(asset_id) DO UPDATE SET line_count=excluded.line_count,
					invalid_json_lines=excluded.invalid_json_lines, invalid_utf8_lines=excluded.invalid_utf8_lines,
					pending_lines=excluded.pending_lines, max_line_bytes=excluded.max_line_bytes,
					top_level_types_json=excluded.top_level_types_json`,
				assetID, file.JSONL.LineCount, file.JSONL.InvalidJSONLines, file.JSONL.InvalidUTF8Lines,
				file.JSONL.PendingLines, file.JSONL.MaxLineBytes, string(typesJSON)); err != nil {
				return SyncResult{}, fmt.Errorf("写入 JSONL summary %s 失败: %w", file.RelativePath, err)
			}
		} else if _, err := tx.ExecContext(ctx, "DELETE FROM jsonl_summaries WHERE asset_id = ?", assetID); err != nil {
			return SyncResult{}, fmt.Errorf("清理非 JSONL summary %s 失败: %w", file.RelativePath, err)
		}
		if file.Kind == domain.AssetKindJSONL {
			result, syncErr := s.syncJSONLIncremental(ctx, tx, workspaceID, report.Root, file, contentSHA256)
			if syncErr != nil {
				return SyncResult{}, syncErr
			}
			switch result.mode {
			case jsonlSyncReused:
				projectionReused++
			case jsonlSyncRebuilt:
				projectionRebuilt++
			case jsonlSyncIncremental:
				projectionIncremental++
			}
			framesAdded += result.framesAdded
			framesReused += result.framesReused
		} else if strings.EqualFold(filepath.Ext(file.RelativePath), ".jsonl") {
			// 类型纠正后清除旧版误建的会话搜索和 framing 投影，物理源文件保留。
			if err := deleteSearchProjection(ctx, tx, assetID); err != nil {
				return SyncResult{}, err
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM jsonl_frames WHERE asset_id = ?", assetID); err != nil {
				return SyncResult{}, fmt.Errorf("清理非对话 JSONL frame 失败: %w", err)
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM jsonl_cursors WHERE asset_id = ?", assetID); err != nil {
				return SyncResult{}, fmt.Errorf("清理非对话 JSONL cursor 失败: %w", err)
			}
		}
	}
	for _, conversation := range report.Conversations {
		diagnosticsJSON, marshalErr := json.Marshal(conversation.Diagnostics)
		if marshalErr != nil {
			return SyncResult{}, fmt.Errorf("序列化对话诊断失败: %w", marshalErr)
		}
		graphJSON, marshalErr := json.Marshal(conversation.AssetGraph)
		if marshalErr != nil {
			return SyncResult{}, fmt.Errorf("序列化对话资产图失败: %w", marshalErr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversations(
				id, workspace_id, source_id, title, first_user_text, cwd, model, model_provider,
				cli_version, started_at, updated_at, event_count, user_messages, assistant_messages,
				turn_count, input_tokens, cached_input_tokens, output_tokens, reasoning_output_tokens,
				total_tokens, status, orphan, diagnostics_json, asset_set_hash, asset_graph_json, last_seen_run_id
			) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				workspace_id=excluded.workspace_id, source_id=excluded.source_id, title=excluded.title,
				first_user_text=excluded.first_user_text, cwd=excluded.cwd, model=excluded.model,
				model_provider=excluded.model_provider, cli_version=excluded.cli_version,
				started_at=excluded.started_at, updated_at=excluded.updated_at, event_count=excluded.event_count,
				user_messages=excluded.user_messages, assistant_messages=excluded.assistant_messages,
				turn_count=excluded.turn_count, input_tokens=excluded.input_tokens,
				cached_input_tokens=excluded.cached_input_tokens, output_tokens=excluded.output_tokens,
				reasoning_output_tokens=excluded.reasoning_output_tokens, total_tokens=excluded.total_tokens,
				status=excluded.status, orphan=excluded.orphan, diagnostics_json=excluded.diagnostics_json,
				asset_set_hash=excluded.asset_set_hash, asset_graph_json=excluded.asset_graph_json, last_seen_run_id=excluded.last_seen_run_id`,
			conversation.ID, workspaceID, nullableString(conversation.SourceID), nullableString(conversation.Title),
			nullableString(conversation.FirstUserText), nullableString(conversation.CWD), nullableString(conversation.Model),
			nullableString(conversation.ModelProvider), nullableString(conversation.CLIVersion), nullableTime(conversation.StartedAt),
			nullableTime(conversation.UpdatedAt), conversation.EventCount, conversation.UserMessages, conversation.AssistantMessages,
			conversation.TurnCount, conversation.TokenUsage.InputTokens, conversation.TokenUsage.CachedInputTokens,
			conversation.TokenUsage.OutputTokens, conversation.TokenUsage.ReasoningOutputTokens, conversation.TokenUsage.TotalTokens,
			conversation.Status, boolInt(conversation.Orphan), string(diagnosticsJSON), nullableString(conversation.AssetSetHash), string(graphJSON), runID); err != nil {
			return SyncResult{}, fmt.Errorf("写入 conversation %s 失败: %w", conversation.ID, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM conversation_assets WHERE conversation_id = ?", conversation.ID); err != nil {
			return SyncResult{}, fmt.Errorf("刷新 conversation 资产关系 %s 失败: %w", conversation.ID, err)
		}
		assetPaths := append([]string{}, conversation.RolloutPaths...)
		assetPaths = append(assetPaths, conversation.RelatedAssetPaths...)
		for _, path := range uniquePaths(assetPaths) {
			if _, ok := currentPaths[path]; !ok {
				// 缺失 rollout 只保留在对话诊断中，不能伪造不存在的资产外键。
				continue
			}
			assetID := stableID("asset", workspaceID+"\x00"+path)
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_assets(conversation_id, asset_id, relation, evidence, confidence) VALUES(?, ?, ?, ?, ?)`, conversation.ID, assetID, "contains", "rollout projection", "high"); err != nil {
				return SyncResult{}, fmt.Errorf("写入 conversation 资产关系 %s/%s 失败: %w", conversation.ID, path, err)
			}
		}
	}
	if len(report.Errors) == 0 {
		if err := s.reconcileMissingAssets(ctx, tx, workspaceID, runID); err != nil {
			return SyncResult{}, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM conversations WHERE workspace_id = ? AND last_seen_run_id <> ?", workspaceID, runID); err != nil {
			return SyncResult{}, fmt.Errorf("清理已消失对话失败: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return SyncResult{}, fmt.Errorf("提交 inventory 事务失败: %w", err)
	}
	committed = true
	// 目录同步与搜索构建分开；清理或暂停搜索数据不会破坏阅读定位游标。
	if len(report.Errors) == 0 {
		paths, err := s.indexSourcePaths(ctx, report.Root)
		if err != nil {
			return SyncResult{}, err
		}
		allowed := make(map[string]bool)
		for _, file := range report.Files {
			if file.Kind == domain.AssetKindJSONL {
				allowed[file.RelativePath] = true
			}
		}
		var removed []string
		for _, path := range paths {
			if !allowed[path] {
				removed = append(removed, path)
			}
		}
		if len(removed) > 0 {
			if err := s.RemoveSearchFiles(ctx, report.Root, removed); err != nil {
				return SyncResult{}, err
			}
		}
	}
	settings, err := s.SearchIndexSettings(ctx)
	if err != nil {
		return SyncResult{}, err
	}
	if allowSearch && settings.Automatic {
		if _, err := s.BuildSearchIndex(ctx, report, nil); err != nil {
			return SyncResult{}, err
		}
	}
	return SyncResult{
		WorkspaceID: workspaceID, RunID: runID, FileCount: len(report.Files), ConversationCount: len(report.Conversations),
		ErrorCount: len(report.Errors), ProjectionReused: projectionReused, ProjectionRebuilt: projectionRebuilt,
		ProjectionIncremental: projectionIncremental, FramesAdded: framesAdded, FramesReused: framesReused,
	}, nil
}

// HasSearchIndex 判断指定工作区是否有覆盖通用内容的索引。
func (s *Store) HasSearchIndex(ctx context.Context, root string) (bool, error) {
	scope, err := s.SearchIndexScope(ctx, root)
	return scope == "all", err
}

// SearchIndexScope 区分通用索引、自选内容索引和无可用索引。
func (s *Store) SearchIndexScope(ctx context.Context, root string) (string, error) {
	var scope string
	err := s.index.db.QueryRowContext(ctx, `SELECT CASE
		WHEN desired<>built OR user_version<>? OR NOT EXISTS(SELECT 1 FROM files WHERE root=? AND committed>0) THEN ''
		WHEN built='' THEN 'all'
		ELSE 'selected'
		END FROM content_scope,pragma_user_version WHERE id=1`, searchProjectionVersion, root).Scan(&scope)
	return scope, err
}

// RemoveConversationDerived 在物理删除已提交后事务化移除本项目保存的对话派生数据。
// 共享资产仍被其他对话引用时只删除当前关系，不删除共享资产和搜索投影。
func (s *Store) RemoveConversationDerived(ctx context.Context, root, conversationID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("archive.db 未初始化")
	}
	if strings.TrimSpace(root) == "" || strings.TrimSpace(conversationID) == "" {
		return fmt.Errorf("清理派生数据需要源根和对话 ID")
	}
	workspaceID := stableID("workspace", root)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始清理对话派生数据事务失败: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	rows, err := tx.QueryContext(ctx, `
		SELECT asset_id FROM conversation_assets
		WHERE conversation_id = ? AND asset_id IN (SELECT id FROM assets WHERE workspace_id = ?)`, conversationID, workspaceID)
	if err != nil {
		return fmt.Errorf("读取对话派生资产失败: %w", err)
	}
	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取对话派生资产 ID 失败: %w", err)
		}
		assetIDs = append(assetIDs, assetID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历对话派生资产失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭对话派生资产查询失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversation_assets WHERE conversation_id = ?`, conversationID); err != nil {
		return fmt.Errorf("删除对话资产关系失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND workspace_id = ?`, conversationID, workspaceID); err != nil {
		return fmt.Errorf("删除对话派生摘要失败: %w", err)
	}
	for _, assetID := range assetIDs {
		var references int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM conversation_assets WHERE asset_id = ?`, assetID).Scan(&references); err != nil {
			return fmt.Errorf("检查共享派生资产失败: %w", err)
		}
		if references > 0 {
			continue
		}
		if err := deleteSearchProjection(ctx, tx, assetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM assets WHERE id = ? AND workspace_id = ?`, assetID, workspaceID); err != nil {
			return fmt.Errorf("删除对话派生资产失败: %w", err)
		}
	}
	// 导航缓存可能含已删除提问的短标题；跨文件引用使精确局部保留不可靠，直接失效可重建缓存。
	reader.DefaultManager.Invalidate()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reader_projections`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交对话派生数据清理失败: %w", err)
	}
	committed = true
	// 依据仍存在的目录记录移除搜索残留；原始文件和共享资产不在此操作范围内。
	paths, err := s.indexSourcePaths(ctx, root)
	if err != nil {
		return err
	}
	var removed []string
	for _, path := range paths {
		var exists int
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM assets WHERE workspace_id=? AND relative_path=?)", workspaceID, path).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			removed = append(removed, path)
		}
	}
	return s.RemoveSearchFiles(ctx, root, removed)
}

// Search 使用当前目录库限定搜索范围；未建立加速索引时仍可按需读取源文件。
func (s *Store) Search(ctx context.Context, root, query string, limit int) ([]domain.SearchHit, error) {
	files, err := s.IndexFilesFromCatalog(ctx, root)
	if err != nil {
		return nil, err
	}
	return s.SearchWithFiles(ctx, root, query, limit, files)
}

type jsonlSyncMode string

const (
	jsonlSyncReused      jsonlSyncMode = "reused"
	jsonlSyncRebuilt     jsonlSyncMode = "rebuilt"
	jsonlSyncIncremental jsonlSyncMode = "incremental"
	jsonlParserVersion                 = 1
)

type jsonlSyncResult struct {
	mode         jsonlSyncMode
	framesAdded  int
	framesReused int
}

type jsonlCursor struct {
	SourceIdentity string
	ObservedSize   int64
	ObservedMTime  int64
	SourceSHA256   string
	PrefixSHA256   string
	Committed      int64
	LineCount      uint64
	ParserVersion  int
	Pending        bool
	State          string
}

// syncJSONLIncremental 将完整 frame、字节范围和哈希写入独立 catalog。
// 源文件始终以只读方式打开；追加复用已提交 frame，截断或前缀变化则局部重建。
func (s *Store) syncJSONLIncremental(ctx context.Context, tx *sql.Tx, workspaceID, root string, file domain.FileInventory, sourceSHA256 string) (jsonlSyncResult, error) {
	result := jsonlSyncResult{}
	if file.Rollout == nil {
		// 没有可识别 rollout 的合成/缺失资产仍可保存 metadata，但不能伪造全文索引。
		return result, nil
	}
	path := filepath.Join(root, filepath.FromSlash(file.RelativePath))
	before, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("读取 JSONL 索引源 %s 失败: %w", file.RelativePath, err)
	}
	if !before.Mode().IsRegular() {
		return result, fmt.Errorf("JSONL 索引源必须是普通文件: %s", file.RelativePath)
	}

	assetID := stableID("asset", workspaceID+"\x00"+file.RelativePath)
	prior, hasPrior, err := readJSONLCursor(ctx, tx, assetID)
	if err != nil {
		return result, err
	}
	if strings.TrimSpace(sourceSHA256) == "" && file.JSONL != nil {
		sourceSHA256 = strings.TrimSpace(file.JSONL.ContentSHA256)
	}

	startOffset := int64(0)
	startLine := uint64(0)
	mode := jsonlSyncRebuilt
	if hasPrior {
		if prior.ParserVersion == jsonlParserVersion && before.Size() == prior.ObservedSize &&
			(strings.TrimSpace(sourceSHA256) == "" || sourceSHA256 == prior.SourceSHA256) {
			currentPrefix, hashErr := hashJSONLPrefix(path, prior.Committed)
			if hashErr == nil && currentPrefix == prior.PrefixSHA256 {
				mode = jsonlSyncReused
				result.framesReused = int(prior.LineCount)
				result.mode = mode
				return result, nil
			}
		}
		if before.Size() >= prior.Committed && prior.Committed >= 0 && prior.ParserVersion == jsonlParserVersion {
			currentPrefix, hashErr := hashJSONLPrefix(path, prior.Committed)
			if hashErr == nil && currentPrefix == prior.PrefixSHA256 {
				mode = jsonlSyncIncremental
				startOffset = prior.Committed
				startLine = prior.LineCount
				result.framesReused = int(prior.LineCount)
			}
		}
	}

	if mode == jsonlSyncRebuilt {
		if err := deleteSearchProjection(ctx, tx, assetID); err != nil {
			return result, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM jsonl_frames WHERE asset_id = ?", assetID); err != nil {
			return result, fmt.Errorf("清理 JSONL frame 索引失败: %w", err)
		}
	}

	input, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("打开 JSONL 索引源 %s 失败: %w", file.RelativePath, err)
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return result, fmt.Errorf("读取 JSONL 索引源状态失败: %w", err)
	}
	capturedSize := opened.Size()
	if startOffset > capturedSize {
		return result, fmt.Errorf("JSONL cursor 超过当前文件大小，要求重建: %s", file.RelativePath)
	}
	if _, err := input.Seek(startOffset, io.SeekStart); err != nil {
		return result, fmt.Errorf("定位 JSONL 增量起点失败: %w", err)
	}

	committedOffset := startOffset
	committedLine := startLine
	pending := false
	reader := io.LimitReader(input, capturedSize-startOffset)
	_, scanErr := catalog.ScanReaderContent(reader, func(frame domain.JSONLFrame, content []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 没有换行的尾部记录可能仍会继续写入，不能把它提交为稳定 frame。
		if frame.LineEnding == "none" {
			pending = true
			return nil
		}
		line := startLine + frame.LineNumber
		start := startOffset + frame.StartOffset
		end := startOffset + frame.EndOffset
		contentEnd := startOffset + frame.ContentEndOffset
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jsonl_frames(asset_id, line, start_offset, end_offset, content_end_offset,
				line_ending, valid_utf8, valid_json, pending, top_type, frame_sha256)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, assetID, line, start, end, contentEnd,
			frame.LineEnding, boolInt(frame.ValidUTF8), boolInt(frame.ValidJSON), boolInt(frame.Pending), frame.TopLevelType, frame.SHA256); err != nil {
			return fmt.Errorf("写入 JSONL frame %s:%d 失败: %w", file.RelativePath, line, err)
		}
		result.framesAdded++

		committedOffset = end
		committedLine = line
		return nil
	})
	if scanErr != nil {
		return result, fmt.Errorf("扫描 JSONL 增量 %s 失败: %w", file.RelativePath, scanErr)
	}

	after, err := os.Stat(path)
	if err != nil {
		return result, fmt.Errorf("复核 JSONL 索引源 %s 失败: %w", file.RelativePath, err)
	}
	if after.Size() < capturedSize {
		return result, fmt.Errorf("JSONL 在索引期间被截断，事务将回滚: %s", file.RelativePath)
	}
	finalCapturedSHA256, hashErr := hashJSONLPrefix(path, capturedSize)
	if hashErr != nil {
		return result, fmt.Errorf("计算 JSONL 内容指纹失败: %w", hashErr)
	}
	if strings.TrimSpace(sourceSHA256) != "" && capturedSize == file.SizeBytes && sourceSHA256 != finalCapturedSHA256 {
		return result, fmt.Errorf("JSONL 在索引期间发生同长度内容重写，事务将回滚: %s", file.RelativePath)
	}
	if strings.TrimSpace(sourceSHA256) == "" || capturedSize != file.SizeBytes {
		sourceSHA256 = finalCapturedSHA256
	}
	prefixSHA256, err := hashJSONLPrefix(path, committedOffset)
	if err != nil {
		return result, fmt.Errorf("计算 JSONL cursor 前缀指纹失败: %w", err)
	}
	state := "stable"
	if after.Size() > capturedSize {
		state = "active_append"
	}
	if pending || committedOffset < capturedSize {
		state = "pending_tail"
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO jsonl_cursors(asset_id, source_identity, observed_size, observed_mtime_ns, source_sha256,
			prefix_sha256, committed_offset, line_count, parser_version, pending, state)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(asset_id) DO UPDATE SET source_identity=excluded.source_identity,
			observed_size=excluded.observed_size, observed_mtime_ns=excluded.observed_mtime_ns,
			source_sha256=excluded.source_sha256, prefix_sha256=excluded.prefix_sha256,
			committed_offset=excluded.committed_offset, line_count=excluded.line_count,
			parser_version=excluded.parser_version, pending=excluded.pending, state=excluded.state`,
		assetID, persistentJSONLIdentity(before), capturedSize, before.ModTime().UnixNano(), sourceSHA256,
		prefixSHA256, committedOffset, committedLine, jsonlParserVersion, boolInt(pending || committedOffset < capturedSize), state); err != nil {
		return result, fmt.Errorf("写入 JSONL cursor 失败: %w", err)
	}
	result.mode = mode
	return result, nil
}

func readJSONLCursor(ctx context.Context, tx *sql.Tx, assetID string) (jsonlCursor, bool, error) {
	var cursor jsonlCursor
	var pending int
	err := tx.QueryRowContext(ctx, `SELECT source_identity, observed_size, observed_mtime_ns, source_sha256,
		prefix_sha256, committed_offset, line_count, parser_version, pending, state
		FROM jsonl_cursors WHERE asset_id = ?`, assetID).Scan(
		&cursor.SourceIdentity, &cursor.ObservedSize, &cursor.ObservedMTime, &cursor.SourceSHA256,
		&cursor.PrefixSHA256, &cursor.Committed, &cursor.LineCount, &cursor.ParserVersion, &pending, &cursor.State)
	if err == sql.ErrNoRows {
		return jsonlCursor{}, false, nil
	}
	if err != nil {
		return jsonlCursor{}, false, fmt.Errorf("读取 JSONL cursor 失败: %w", err)
	}
	cursor.Pending = pending != 0
	return cursor, true, nil
}

func hashJSONLPrefix(path string, length int64) (string, error) {
	if length < 0 {
		return "", fmt.Errorf("JSONL cursor 长度不能为负数")
	}
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if length > info.Size() {
		return "", fmt.Errorf("JSONL cursor 长度 %d 超过文件大小 %d", length, info.Size())
	}
	digest := sha256.New()
	if _, err := io.CopyN(digest, input, length); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func persistentJSONLIdentity(info os.FileInfo) string {
	// Go 的 FileInfo 没有跨平台统一 inode 字段；前缀哈希承担内容一致性校验，
	// 这里保存模式和名称作为诊断信息，不把它当作唯一安全凭据。
	return fmt.Sprintf("%s:%s", info.Mode().String(), info.Name())
}

func deleteSearchProjection(ctx context.Context, tx *sql.Tx, assetID string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_documents WHERE asset_id IN (SELECT doc_id FROM search_locations WHERE asset_id = ?)", assetID); err != nil {
		return fmt.Errorf("清理搜索文档失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM search_locations WHERE asset_id = ?", assetID); err != nil {
		return fmt.Errorf("清理搜索定位失败: %w", err)
	}
	return nil
}

func uniquePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok || strings.TrimSpace(path) == "" {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func (s *Store) reconcileMissingAssets(ctx context.Context, tx *sql.Tx, workspaceID, runID string) error {
	rows, err := tx.QueryContext(ctx, "SELECT id FROM assets WHERE workspace_id = ? AND last_seen_run_id <> ?", workspaceID, runID)
	if err != nil {
		return fmt.Errorf("查找已消失资产失败: %w", err)
	}
	assetIDs := make([]string, 0)
	for rows.Next() {
		var assetID string
		if err := rows.Scan(&assetID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("读取已消失资产失败: %w", err)
		}
		assetIDs = append(assetIDs, assetID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("遍历已消失资产失败: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("关闭已消失资产查询失败: %w", err)
	}
	for _, assetID := range assetIDs {
		if err := deleteSearchProjection(ctx, tx, assetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM assets WHERE id = ?", assetID); err != nil {
			return fmt.Errorf("删除已消失资产失败: %w", err)
		}
	}
	return nil
}

func (s *Store) configure(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("启用 SQLite 外键失败: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return fmt.Errorf("设置 SQLite busy timeout 失败: %w", err)
	}
	if path != ":memory:" {
		if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			return fmt.Errorf("启用 archive.db WAL 失败: %w", err)
		}
	}
	return nil
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始数据库迁移失败: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("创建迁移表失败: %w", err)
	}
	version, err := migrationVersion(ctx, tx)
	if err != nil {
		return err
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("数据库版本 %d 高于程序支持版本 %d", version, currentSchemaVersion)
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE workspaces (
				id TEXT PRIMARY KEY,
				root_path TEXT NOT NULL UNIQUE,
				adapter_id TEXT NOT NULL,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			);
			CREATE TABLE scan_runs (
				id TEXT PRIMARY KEY,
				workspace_id TEXT NOT NULL REFERENCES workspaces(id),
				started_at TEXT NOT NULL,
				completed_at TEXT NOT NULL,
				read_only INTEGER NOT NULL CHECK(read_only = 1),
				file_count INTEGER NOT NULL,
				error_count INTEGER NOT NULL
			);
			CREATE TABLE assets (
				id TEXT PRIMARY KEY,
				workspace_id TEXT NOT NULL REFERENCES workspaces(id),
				relative_path TEXT NOT NULL,
				kind TEXT NOT NULL,
				size_bytes INTEGER NOT NULL,
				mod_time_ns INTEGER NOT NULL,
				status TEXT NOT NULL,
				error TEXT,
				changed_during_scan INTEGER NOT NULL CHECK(changed_during_scan IN (0, 1)),
				first_seen_run_id TEXT NOT NULL REFERENCES scan_runs(id),
				last_seen_run_id TEXT NOT NULL REFERENCES scan_runs(id),
				UNIQUE(workspace_id, relative_path)
			);
			CREATE TABLE jsonl_summaries (
				asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
				line_count INTEGER NOT NULL,
				invalid_json_lines INTEGER NOT NULL,
				invalid_utf8_lines INTEGER NOT NULL,
				pending_lines INTEGER NOT NULL,
				max_line_bytes INTEGER NOT NULL,
				top_level_types_json TEXT NOT NULL
			);
			CREATE INDEX assets_workspace_status_idx ON assets(workspace_id, status);
			CREATE INDEX assets_workspace_mtime_idx ON assets(workspace_id, mod_time_ns);
			CREATE VIRTUAL TABLE search_documents USING fts5(
				asset_id UNINDEXED,
				source_path UNINDEXED,
				role,
				content
			);`); err != nil {
			return fmt.Errorf("创建 archive.db 初始 schema 失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)", initialSchemaVersion, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("记录初始迁移失败: %w", err)
		}
		version = initialSchemaVersion
	}
	if version < 2 {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE conversations (
				id TEXT PRIMARY KEY,
				workspace_id TEXT NOT NULL REFERENCES workspaces(id),
				source_id TEXT,
				title TEXT,
				first_user_text TEXT,
				cwd TEXT,
				model TEXT,
				model_provider TEXT,
				cli_version TEXT,
				started_at TEXT,
				updated_at TEXT,
				event_count INTEGER NOT NULL DEFAULT 0,
				user_messages INTEGER NOT NULL DEFAULT 0,
				assistant_messages INTEGER NOT NULL DEFAULT 0,
				turn_count INTEGER NOT NULL DEFAULT 0,
				input_tokens INTEGER NOT NULL DEFAULT 0,
				cached_input_tokens INTEGER NOT NULL DEFAULT 0,
				output_tokens INTEGER NOT NULL DEFAULT 0,
				reasoning_output_tokens INTEGER NOT NULL DEFAULT 0,
				total_tokens INTEGER NOT NULL DEFAULT 0,
				status TEXT NOT NULL,
				orphan INTEGER NOT NULL CHECK(orphan IN (0, 1)),
				diagnostics_json TEXT NOT NULL,
				asset_set_hash TEXT,
				last_seen_run_id TEXT NOT NULL REFERENCES scan_runs(id)
			);
			CREATE TABLE conversation_assets (
				conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
				asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
				relation TEXT NOT NULL,
				evidence TEXT NOT NULL,
				confidence TEXT NOT NULL,
				PRIMARY KEY(conversation_id, asset_id, relation)
			);
			CREATE INDEX conversations_workspace_updated_idx ON conversations(workspace_id, updated_at DESC);
			CREATE INDEX conversations_workspace_status_idx ON conversations(workspace_id, status);
			INSERT INTO schema_migrations(version, applied_at) VALUES(2, ?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("创建 conversation schema 失败: %w", err)
		}
		version = 2
	}
	if version < 3 {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE search_locations (
				doc_id TEXT PRIMARY KEY,
				workspace_id TEXT NOT NULL REFERENCES workspaces(id),
				asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
				conversation_id TEXT,
				source_path TEXT NOT NULL,
				line INTEGER NOT NULL,
				start_offset INTEGER NOT NULL,
				end_offset INTEGER NOT NULL,
				top_type TEXT NOT NULL,
				snippet TEXT NOT NULL
			);
			CREATE INDEX search_locations_workspace_path_idx ON search_locations(workspace_id, source_path);
			INSERT INTO schema_migrations(version, applied_at) VALUES(3, ?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("创建搜索定位 schema 失败: %w", err)
		}
		version = 3
	}
	if version < 4 {
		var columnCount int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('assets') WHERE name = ?", "content_sha256").Scan(&columnCount); err != nil {
			return fmt.Errorf("检查资产内容指纹字段失败: %w", err)
		}
		if columnCount == 0 {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE assets ADD COLUMN content_sha256 TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("增加资产内容指纹字段失败: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(4, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("记录内容指纹迁移失败: %w", err)
		}
		version = 4
	}
	if version < 5 {
		var columnCount int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pragma_table_info('conversations') WHERE name = ?", "asset_graph_json").Scan(&columnCount); err != nil {
			return fmt.Errorf("检查对话资产图字段失败: %w", err)
		}
		if columnCount == 0 {
			if _, err := tx.ExecContext(ctx, "ALTER TABLE conversations ADD COLUMN asset_graph_json TEXT NOT NULL DEFAULT ''"); err != nil {
				return fmt.Errorf("增加对话资产图字段失败: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(5, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("记录对话资产图迁移失败: %w", err)
		}
		version = 5
	}
	if version < 6 {
		// 搜索投影的正文现在使用共享规范化规则；旧投影无法可靠区分版本，
		// 清空后由下一次只读 catalog sync 按新规则重建。
		if _, err := tx.ExecContext(ctx, "DELETE FROM search_locations; DELETE FROM search_documents;"); err != nil {
			return fmt.Errorf("清理旧搜索投影失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(6, ?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("记录搜索投影迁移失败: %w", err)
		}
		version = 6
	}
	if version < 7 {
		if _, err := tx.ExecContext(ctx, `
			CREATE INDEX IF NOT EXISTS search_locations_asset_idx ON search_locations(asset_id);
			INSERT INTO schema_migrations(version, applied_at) VALUES(7, ?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("创建搜索资产定位索引失败: %w", err)
		}
		version = 7
	}
	if version < 8 {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS jsonl_frames (
				asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
				line INTEGER NOT NULL CHECK(line > 0),
				start_offset INTEGER NOT NULL CHECK(start_offset >= 0),
				end_offset INTEGER NOT NULL CHECK(end_offset > start_offset),
				content_end_offset INTEGER NOT NULL CHECK(content_end_offset >= start_offset AND content_end_offset <= end_offset),
				line_ending TEXT NOT NULL,
				valid_utf8 INTEGER NOT NULL CHECK(valid_utf8 IN (0, 1)),
				valid_json INTEGER NOT NULL CHECK(valid_json IN (0, 1)),
				pending INTEGER NOT NULL CHECK(pending IN (0, 1)),
				top_type TEXT NOT NULL,
				frame_sha256 TEXT NOT NULL,
				PRIMARY KEY(asset_id, line)
			);
			CREATE INDEX IF NOT EXISTS jsonl_frames_asset_offset_idx ON jsonl_frames(asset_id, start_offset);
			CREATE TABLE IF NOT EXISTS jsonl_cursors (
				asset_id TEXT PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
				source_identity TEXT NOT NULL,
				observed_size INTEGER NOT NULL CHECK(observed_size >= 0),
				observed_mtime_ns INTEGER NOT NULL,
				source_sha256 TEXT NOT NULL,
				prefix_sha256 TEXT NOT NULL,
				committed_offset INTEGER NOT NULL CHECK(committed_offset >= 0 AND committed_offset <= observed_size),
				line_count INTEGER NOT NULL CHECK(line_count >= 0),
				parser_version INTEGER NOT NULL,
				pending INTEGER NOT NULL CHECK(pending IN (0, 1)),
				state TEXT NOT NULL
			);
			INSERT INTO schema_migrations(version, applied_at) VALUES(8, ?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("创建 JSONL 增量 catalog schema 失败: %w", err)
		}
		version = 8
	}
	if version < 9 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS reader_projections (
			scope TEXT PRIMARY KEY, stamp TEXT NOT NULL, payload BLOB NOT NULL,
			updated_at TEXT NOT NULL);
			INSERT INTO schema_migrations(version, applied_at) VALUES(9, ?);`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("创建阅读导航缓存失败: %w", err)
		}
	}
	// 每次打开都幂等补齐关键索引，兼容旧版本曾手动删除迁移记录的数据库。
	if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS search_locations_asset_idx ON search_locations(asset_id)"); err != nil {
		return fmt.Errorf("补齐搜索资产定位索引失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交数据库迁移失败: %w", err)
	}
	return nil
}

func migrationVersion(ctx context.Context, tx *sql.Tx) (int, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return 0, fmt.Errorf("读取迁移版本失败: %w", err)
	}
	return version, nil
}

func prepareDSN(path string) (string, error) {
	if path == ":memory:" {
		return path, nil
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("archive.db 路径不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析 archive.db 路径失败: %w", err)
	}
	for _, part := range strings.Split(filepath.Clean(abs), string(filepath.Separator)) {
		if part == ".codex" {
			return "", fmt.Errorf("拒绝把 archive.db 放入原始 .codex 目录: %s", abs)
		}
	}
	if parent := filepath.Dir(abs); parent != "." {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return "", fmt.Errorf("创建 archive.db 父目录失败: %w", err)
		}
	}
	return abs, nil
}

func stableID(prefix, value string) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + "-" + hex.EncodeToString(digest[:16])
}

func randomID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("生成操作 ID 失败: %w", err)
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}
