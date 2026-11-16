// 中文说明：验证 archive.db 迁移、FTS5 能力、事务同步和源目录隔离边界。
package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
)

func TestOpenMigratesControlPlaneAndSupportsFTS5(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	archive, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open returned error: %v", err)
	}
	defer archive.Close()
	version, err := archive.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("unexpected schema version: %d", version)
	}
	var count int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM search_documents").Scan(&count); err != nil {
		t.Fatalf("FTS5 unavailable: %v", err)
	}
	if _, err := archive.db.ExecContext(ctx, "INSERT INTO search_documents(asset_id, source_path, role, content) VALUES(?, ?, ?, ?)", "a", "one.jsonl", "user", "Codex Chronicle"); err != nil {
		t.Fatal(err)
	}
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM search_documents WHERE search_documents MATCH ?", "Chronicle").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unexpected FTS5 result count: %d", count)
	}
	var indexCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM pragma_index_list('search_locations') WHERE name = ?", "search_locations_asset_idx").Scan(&indexCount); err != nil {
		t.Fatalf("search location asset index is unavailable: %v", err)
	}
	if indexCount != 1 {
		t.Fatalf("search location asset index is missing: %d", indexCount)
	}
	rows, err := archive.db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM search_locations WHERE asset_id = ?)", "asset-test")
	if err != nil {
		t.Fatalf("search location asset query plan failed: %v", err)
	}
	usesAssetIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		usesAssetIndex = usesAssetIndex || strings.Contains(detail, "search_locations_asset_idx")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !usesAssetIndex {
		t.Fatal("projection reuse query does not use the search location asset index")
	}
}

func TestPreflightPathIsReadOnlyAndDetectsMigrationState(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version, applied_at) VALUES(1, 'test');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	preflight, err := PreflightPath(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if preflight.CurrentVersion != 1 || preflight.SupportedVersion != currentSchemaVersion || preflight.Status != "migration_required" || !preflight.RequiresMigration || preflight.FutureVersion || !preflight.ReadOnly {
		t.Fatalf("unexpected legacy preflight: %+v", preflight)
	}
	after, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("read-only migration preflight changed database bytes")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(databasePath + suffix); !os.IsNotExist(err) {
			t.Fatalf("read-only migration preflight created %s: %v", suffix, err)
		}
	}
}

func TestPreflightPathBlocksFutureSchemaWithoutWriting(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "future.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version, applied_at) VALUES(999, 'future');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	preflight, err := PreflightPath(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Status != "future_schema" || !preflight.FutureVersion || preflight.RequiresMigration || !preflight.ReadOnly {
		t.Fatalf("unexpected future preflight: %+v", preflight)
	}
}

func TestPreflightPathBlocksImmutableViewWithAuxiliaryFiles(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "wal.db")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version, applied_at) VALUES(6, 'test');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("pending-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	preflight, err := PreflightPath(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if preflight.Status != "auxiliary_present" || len(preflight.AuxiliaryFiles) != 1 || !preflight.ReadOnly {
		t.Fatalf("auxiliary files did not block preflight: %+v", preflight)
	}
}

func TestSyncInventoryWritesDerivedDataOnly(t *testing.T) {
	ctx := context.Background()
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	modTime := time.Unix(123, 456).UTC()
	report := domain.InventoryReport{
		SchemaVersion: 1,
		Root:          "/tmp/synthetic-codex",
		ReadOnly:      true,
		GeneratedAt:   time.Unix(100, 0).UTC(),
		Files: []domain.FileInventory{
			{
				Asset: domain.Asset{RelativePath: "sessions/one.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 42, ModTime: modTime, Status: domain.StatusStable},
				JSONL: &domain.JSONLScanSummary{LineCount: 2, MaxLineBytes: 24, TopLevelTypes: map[string]uint64{"session_meta": 1, "response_item": 1}},
			},
		},
	}
	result, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 1 || result.ErrorCount != 0 || result.WorkspaceID == "" || result.RunID == "" {
		t.Fatalf("unexpected sync result: %+v", result)
	}
	var path string
	if err := archive.db.QueryRowContext(ctx, "SELECT relative_path FROM assets").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if path != "sessions/one.jsonl" {
		t.Fatalf("unexpected asset path: %s", path)
	}
	var lineCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT line_count FROM jsonl_summaries").Scan(&lineCount); err != nil {
		t.Fatal(err)
	}
	if lineCount != 2 {
		t.Fatalf("unexpected line count: %d", lineCount)
	}
}

func TestSyncInventoryPersistsConversationGraphProjection(t *testing.T) {
	ctx := context.Background()
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	report := domain.InventoryReport{
		SchemaVersion: 1, Root: "/tmp/conversation-root", ReadOnly: true,
		GeneratedAt: time.Unix(200, 0).UTC(),
		Files: []domain.FileInventory{
			{
				Asset: domain.Asset{RelativePath: "sessions/a.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: 10, ModTime: time.Unix(201, 0).UTC(), Status: domain.StatusStable},
				JSONL: &domain.JSONLScanSummary{LineCount: 1, TopLevelTypes: map[string]uint64{"session_meta": 1}},
			},
			{Asset: domain.Asset{RelativePath: "state_5.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: 20, ModTime: time.Unix(202, 0).UTC(), Status: domain.StatusStable}},
		},
		Conversations: []domain.ConversationSummary{{
			ID: "conversation-test", SourceID: "thread-test", Title: "测试对话", CWD: "/tmp/project",
			RolloutPaths: []string{"sessions/a.jsonl"}, RelatedAssetPaths: []string{"state_5.sqlite"}, Status: domain.StatusStable,
			AssetSetHash: "hash-test", TokenUsage: domain.TokenUsage{TotalTokens: 8},
			AssetGraph: &domain.ConversationAssetGraph{Version: 1, ConversationID: "conversation-test", Completeness: "complete", Hash: "graph-test"},
		}},
	}
	result, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if result.ConversationCount != 1 {
		t.Fatalf("unexpected conversation count: %+v", result)
	}
	var title string
	if err := archive.db.QueryRowContext(ctx, "SELECT title FROM conversations WHERE id = ?", "conversation-test").Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "测试对话" {
		t.Fatalf("unexpected conversation title: %q", title)
	}
	var graphJSON string
	if err := archive.db.QueryRowContext(ctx, "SELECT asset_graph_json FROM conversations WHERE id = ?", "conversation-test").Scan(&graphJSON); err != nil {
		t.Fatal(err)
	}
	if graphJSON == "" || graphJSON == "null" || !strings.Contains(graphJSON, "graph-test") {
		t.Fatalf("conversation asset graph was not persisted: %q", graphJSON)
	}
	var relationCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM conversation_assets WHERE conversation_id = ?", "conversation-test").Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if relationCount != 2 {
		t.Fatalf("unexpected graph relation count: %d", relationCount)
	}
}

func TestSyncInventoryBuildsFTSLocationsAndSearches(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"search-thread\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"需要索引的关键词\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	firstSync, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if firstSync.ProjectionRebuilt != 1 || firstSync.ProjectionReused != 0 {
		t.Fatalf("unexpected first projection sync: %+v", firstSync)
	}
	secondSync, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if secondSync.ProjectionRebuilt != 0 || secondSync.ProjectionReused != 1 {
		t.Fatalf("unchanged JSONL projection was not reused: %+v", secondSync)
	}
	indexed, err := archive.HasSearchIndex(ctx, root)
	if err != nil || !indexed {
		t.Fatalf("expected search index: indexed=%v err=%v", indexed, err)
	}
	hits, err := archive.Search(ctx, root, "索引的关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RelativePath != "rollout.jsonl" || hits[0].Line != 2 || hits[0].StartOffset <= 0 {
		t.Fatalf("unexpected FTS locations: %+v", hits)
	}
}

func TestSyncInventoryIncrementallyAppendsAndRebuildsJSONLFrames(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	first := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"incremental-thread\"}}\n"
	second := "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"input_text\":\"初始关键词\"}]}}\n"
	third := "{\"type\":\"event_msg\",\"payload\":{\"text\":\"追加关键词\"}}\n"
	if err := os.WriteFile(path, []byte(first+second), 0o644); err != nil {
		t.Fatal(err)
	}
	scanner := application.NewScanner()
	report, err := scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	initial, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if initial.ProjectionRebuilt != 1 || initial.FramesAdded != 2 {
		t.Fatalf("unexpected initial incremental catalog result: %+v", initial)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(third); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	report, err = scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	appended, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if appended.ProjectionIncremental != 1 || appended.ProjectionRebuilt != 0 || appended.FramesAdded != 1 || appended.FramesReused != 2 {
		t.Fatalf("append did not reuse the committed prefix: %+v", appended)
	}
	var frameCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM jsonl_frames").Scan(&frameCount); err != nil {
		t.Fatal(err)
	}
	if frameCount != 3 {
		t.Fatalf("unexpected frame count after append: %d", frameCount)
	}
	hits, err := archive.Search(ctx, root, "追加关键词", 10)
	if err != nil || len(hits) != 1 || hits[0].Line != 3 {
		t.Fatalf("appended frame was not searchable: hits=%+v err=%v", hits, err)
	}

	rewritten := first + "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"input_text\":\"重写关键词\"}]}}\n"
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err = scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.ProjectionRebuilt != 1 || rebuilt.ProjectionIncremental != 0 || rebuilt.FramesAdded != 2 {
		t.Fatalf("rewrite did not invalidate the old catalog: %+v", rebuilt)
	}
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM jsonl_frames").Scan(&frameCount); err != nil {
		t.Fatal(err)
	}
	if frameCount != 2 {
		t.Fatalf("old frames remained after rewrite: %d", frameCount)
	}
	oldHits, err := archive.Search(ctx, root, "追加关键词", 10)
	if err != nil || len(oldHits) != 0 {
		t.Fatalf("old search projection remained after rewrite: hits=%+v err=%v", oldHits, err)
	}
	newHits, err := archive.Search(ctx, root, "重写关键词", 10)
	if err != nil || len(newHits) != 1 || newHits[0].Line != 2 {
		t.Fatalf("rewritten frame was not searchable: hits=%+v err=%v", newHits, err)
	}
}

func TestSyncInventoryKeepsActiveJSONLTailOutOfCommittedFrames(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "active.jsonl")
	complete := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"active-thread\"}}\n"
	if err := os.WriteFile(path, []byte(complete+"{\"type\":\"response_item\""), 0o644); err != nil {
		t.Fatal(err)
	}
	scanner := application.NewScanner()
	report, err := scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	first, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if first.FramesAdded != 1 {
		t.Fatalf("incomplete tail was committed: %+v", first)
	}
	var committed, observed int64
	var pending int
	if err := archive.db.QueryRowContext(ctx, "SELECT committed_offset, observed_size, pending FROM jsonl_cursors").Scan(&committed, &observed, &pending); err != nil {
		t.Fatal(err)
	}
	if committed >= observed || pending != 1 {
		t.Fatalf("unexpected active cursor: committed=%d observed=%d pending=%d", committed, observed, pending)
	}

	if file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644); err != nil {
		t.Fatal(err)
	} else {
		if _, err := file.WriteString(`,"payload":{"text":"完成关键词"}}` + "\n"); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	report, err = scanner.Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatal(err)
	}
	if second.ProjectionIncremental != 1 || second.FramesAdded != 1 {
		t.Fatalf("completed tail was not incrementally committed: %+v", second)
	}
	var frameCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM jsonl_frames").Scan(&frameCount); err != nil {
		t.Fatal(err)
	}
	if frameCount != 2 {
		t.Fatalf("unexpected frame count after completing tail: %d", frameCount)
	}
}

func TestSearchUsesSharedWhitespaceNormalizationForCatalog(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"normalized-search-thread\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"input_text\":\"第一行\\n第二行\"}]}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := archive.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	hits, err := archive.Search(ctx, root, "第一行   第二行", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 || hits[0].StartOffset <= 0 {
		t.Fatalf("catalog search did not normalize JSON newline and whitespace: %+v", hits)
	}
	if !strings.Contains(hits[0].Snippet, "第一行") || !strings.Contains(hits[0].Snippet, "第二行") {
		t.Fatalf("search evidence lost original snippet: %+v", hits[0])
	}
}

func TestSearchProjectionMigrationRebuildsWithSharedNormalization(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"migration-thread\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"input_text\":\"迁移搜索词\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "archive.db")
	archive, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.SyncInventory(ctx, report); err != nil {
		_ = archive.Close()
		t.Fatal(err)
	}
	if indexed, err := archive.HasSearchIndex(ctx, root); err != nil || !indexed {
		_ = archive.Close()
		t.Fatalf("expected initial search projection: indexed=%v err=%v", indexed, err)
	}
	if _, err := archive.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version >= 6"); err != nil {
		_ = archive.Close()
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if indexed, err := reopened.HasSearchIndex(ctx, root); err != nil || indexed {
		t.Fatalf("search projection was not invalidated by migration: indexed=%v err=%v", indexed, err)
	}
	if version, err := reopened.SchemaVersion(ctx); err != nil || version != currentSchemaVersion {
		t.Fatalf("unexpected migrated schema version: version=%d err=%v", version, err)
	}
	if _, err := reopened.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	if indexed, err := reopened.HasSearchIndex(ctx, root); err != nil || !indexed {
		t.Fatalf("search projection was not rebuilt after migration: indexed=%v err=%v", indexed, err)
	}
}

func TestSearchLocationAssetIndexMigrationPreservesProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"asset-index-thread\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"input_text\":\"资产索引迁移搜索词\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "archive.db")
	archive, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.SyncInventory(ctx, report); err != nil {
		_ = archive.Close()
		t.Fatal(err)
	}
	if _, err := archive.db.ExecContext(ctx, "DROP INDEX search_locations_asset_idx; DELETE FROM schema_migrations WHERE version = 7"); err != nil {
		_ = archive.Close()
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	hits, err := migrated.Search(ctx, root, "资产索引迁移搜索词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 {
		t.Fatalf("asset index migration changed the existing search projection: %+v", hits)
	}
	rows, err := migrated.db.QueryContext(ctx, "EXPLAIN QUERY PLAN SELECT EXISTS(SELECT 1 FROM search_locations WHERE asset_id = ?)", "asset-test")
	if err != nil {
		t.Fatal(err)
	}
	usesAssetIndex := false
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		usesAssetIndex = usesAssetIndex || strings.Contains(detail, "search_locations_asset_idx")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if !usesAssetIndex {
		t.Fatal("migrated projection reuse query does not use the search location asset index")
	}
}

func TestSyncInventoryReconcilesRewrittenAndRemovedAssets(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	writeRollout := func(text string) {
		t.Helper()
		content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"sync-thread\"}}\n" +
			"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"" + text + "\"}]}}\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeRollout("旧搜索内容")
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := archive.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	hits, err := archive.Search(ctx, root, "旧搜索内容", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("expected initial search hit: hits=%+v err=%v", hits, err)
	}

	writeRollout("新搜索内容")
	rewritten, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.SyncInventory(ctx, rewritten); err != nil {
		t.Fatal(err)
	}
	oldHits, err := archive.Search(ctx, root, "旧搜索内容", 10)
	if err != nil || len(oldHits) != 0 {
		t.Fatalf("stale search projection remained: hits=%+v err=%v", oldHits, err)
	}
	newHits, err := archive.Search(ctx, root, "新搜索内容", 10)
	if err != nil || len(newHits) != 1 {
		t.Fatalf("rewritten search projection missing: hits=%+v err=%v", newHits, err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	removed, err := application.NewScanner().Scan(ctx, root)
	if err != nil || len(removed.Errors) != 0 {
		t.Fatalf("unexpected empty scan: report=%+v err=%v", removed, err)
	}
	if _, err := archive.SyncInventory(ctx, removed); err != nil {
		t.Fatal(err)
	}
	assetCount, err := archive.AssetCount(ctx)
	if err != nil || assetCount != 0 {
		t.Fatalf("removed asset remained in catalog: count=%d err=%v", assetCount, err)
	}
	removedHits, err := archive.Search(ctx, root, "新搜索内容", 10)
	if err != nil || len(removedHits) != 0 {
		t.Fatalf("removed search projection remained: hits=%+v err=%v", removedHits, err)
	}
	var conversationCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM conversations").Scan(&conversationCount); err != nil {
		t.Fatal(err)
	}
	if conversationCount != 0 {
		t.Fatalf("removed conversation remained in catalog: count=%d", conversationCount)
	}
}

func TestRemoveConversationDerivedKeepsSharedAssetsUntilLastReference(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"shared-thread\"}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"共享派生内容\"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Conversations) != 1 {
		t.Fatalf("unexpected scanner conversations: %+v", report.Conversations)
	}
	other := report.Conversations[0]
	other.ID = "conversation-shared-other"
	other.SourceID = "shared-other"
	report.Conversations = append(report.Conversations, other)
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	if _, err := archive.SyncInventory(ctx, report); err != nil {
		t.Fatal(err)
	}
	firstID := report.Conversations[0].ID
	if err := archive.RemoveConversationDerived(ctx, root, firstID); err != nil {
		t.Fatal(err)
	}
	assetCount, err := archive.AssetCount(ctx)
	if err != nil || assetCount != 1 {
		t.Fatalf("shared asset was removed too early: count=%d err=%v", assetCount, err)
	}
	var conversationCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM conversations").Scan(&conversationCount); err != nil {
		t.Fatal(err)
	}
	if conversationCount != 1 {
		t.Fatalf("unexpected remaining conversation count: %d", conversationCount)
	}
	hits, err := archive.Search(ctx, root, "共享派生内容", 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("shared search projection disappeared: hits=%+v err=%v", hits, err)
	}
	if err := archive.RemoveConversationDerived(ctx, root, other.ID); err != nil {
		t.Fatal(err)
	}
	assetCount, err = archive.AssetCount(ctx)
	if err != nil || assetCount != 0 {
		t.Fatalf("last shared asset remained: count=%d err=%v", assetCount, err)
	}
	hits, err = archive.Search(ctx, root, "共享派生内容", 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("search projection remained after last reference: hits=%+v err=%v", hits, err)
	}
}

func TestSyncInventorySkipsMissingRolloutForeignKey(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-a\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", filepath.Join(root, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT, first_user_message TEXT, cwd TEXT, tokens_used INTEGER, archived INTEGER);
		INSERT INTO threads VALUES('thread-a','sessions/one.jsonl','存在','', '/project',1,0);
		INSERT INTO threads VALUES('thread-missing','sessions/missing.jsonl','缺失','', '/project',2,0);`); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := application.NewScanner().Scan(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	result, err := archive.SyncInventory(ctx, report)
	if err != nil {
		t.Fatalf("missing rollout should remain a readable conflict: %v", err)
	}
	if result.ConversationCount != 2 {
		t.Fatalf("unexpected conversation count: %+v", result)
	}
	var relationCount int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM conversation_assets").Scan(&relationCount); err != nil {
		t.Fatal(err)
	}
	if relationCount != 3 {
		t.Fatalf("expected existing rollout plus SQLite evidence relations, got %d", relationCount)
	}
}

func TestSyncInventoryRejectsNonReadOnlyReport(t *testing.T) {
	archive, err := Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	report := domain.InventoryReport{Root: "/tmp/source", ReadOnly: false}
	if _, err := archive.SyncInventory(context.Background(), report); err == nil {
		t.Fatal("expected non-read-only report to be rejected")
	}
}

func TestOpenRejectsCodexPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".codex", "archive.db")
	if _, err := Open(context.Background(), path); err == nil {
		t.Fatal("expected .codex path to be rejected")
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var migrations int
	if err := second.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&migrations); err != nil {
		t.Fatal(err)
	}
	if migrations != currentSchemaVersion {
		t.Fatalf("unexpected migration count: %d", migrations)
	}
	if _, err := second.db.ExecContext(ctx, "SELECT 1"); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
}

func TestOpenMigratesExistingV1ControlPlaneToCurrentSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, `
		DROP TABLE search_locations;
		DROP TABLE conversation_assets;
		DROP TABLE conversations;
		DELETE FROM schema_migrations WHERE version > 1;`); err != nil {
		first.Close()
		t.Fatalf("failed to prepare v1 database: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening v1 database failed: %v", err)
	}
	defer second.Close()
	version, err := second.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != currentSchemaVersion {
		t.Fatalf("unexpected migrated schema version: %d", version)
	}
	for _, table := range []string{"conversations", "conversation_assets", "search_locations"} {
		var count int
		if err := second.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("migrated table %s is missing", table)
		}
	}
}

func TestOpenCreatesMigrationBackupAndCanRestore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.ExecContext(ctx, `
		DROP TABLE search_locations;
		DROP TABLE conversation_assets;
		DROP TABLE conversations;
		DELETE FROM schema_migrations WHERE version > 1;`); err != nil {
		_ = first.Close()
		t.Fatalf("failed to prepare v1 database: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("opening v1 database with backup failed: %v", err)
	}
	backup, ok := migrated.MigrationBackup()
	if !ok || backup.FromVersion != 1 || backup.ToVersion != currentSchemaVersion || backup.Path == "" {
		_ = migrated.Close()
		t.Fatalf("unexpected migration backup: %+v, ok=%v", backup, ok)
	}
	if backup.SourcePath != path || backup.SizeBytes <= 0 || len(backup.SHA256) != 64 {
		_ = migrated.Close()
		t.Fatalf("migration backup metadata is incomplete: %+v", backup)
	}
	if _, err := os.Stat(backup.Path); err != nil {
		_ = migrated.Close()
		t.Fatalf("migration backup does not exist: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	if err := RestoreMigrationBackup(ctx, path, backup.Path); err != nil {
		t.Fatalf("restoring migration backup failed: %v", err)
	}
	restored, err := PreflightPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != "migration_required" || restored.CurrentVersion != 1 {
		t.Fatalf("restore did not return database to v1: %+v", restored)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening restored database failed: %v", err)
	}
	defer reopened.Close()
	if version, err := reopened.SchemaVersion(ctx); err != nil || version != currentSchemaVersion {
		t.Fatalf("restored database did not migrate again: version=%d err=%v", version, err)
	}
}

func TestOpenRestoresMigrationBackupWhenMigrationFails(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "archive.db")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(ctx, "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL); INSERT INTO schema_migrations(version, applied_at) VALUES(5, 'test');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "已自动恢复迁移前备份") || !strings.Contains(err.Error(), "archive.db 迁移失败") {
		t.Fatalf("expected migration failure with automatic restore, got %v", err)
	}
	preflight, err := PreflightPath(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if preflight.CurrentVersion != 5 || preflight.Status != "migration_required" {
		t.Fatalf("automatic restore did not preserve the old schema: %+v", preflight)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, statErr := os.Lstat(path + suffix); !os.IsNotExist(statErr) {
			t.Fatalf("automatic restore left SQLite %s file: %v", suffix, statErr)
		}
	}
}
