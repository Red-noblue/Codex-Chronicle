// 中文说明：验证活动副本的资产级结果、JSONL 前缀发布和 SQLite WAL online backup。
package copy

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/sqliteguard"
	_ "modernc.org/sqlite"
)

type liveFixedProcessDetector struct {
	files map[string][]domain.ProcessHandle
}

func (detector liveFixedProcessDetector) Detect(context.Context, string) activity.ProcessSnapshot {
	return activity.ProcessSnapshot{Available: true, Reason: "fixture process probe", Files: detector.files}
}

func TestPrepareAndExecuteLivePublishesStableAndActiveJSONLAssets(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "live-copy")
	stablePath := filepath.Join(source, "sessions", "stable.jsonl")
	activePath := filepath.Join(source, "sessions", "active.jsonl")
	writeCopyTestFile(t, stablePath, `{"type":"stable"}`+"\n")
	writeCopyTestFile(t, activePath, `{"type":"complete"}`+"\n"+`{"type":"pending"`)
	activeBefore, activeBeforeInfo := snapshotOptionalFile(t, activePath)
	options := &activity.Options{
		Interval:        time.Millisecond,
		ProcessDetector: liveFixedProcessDetector{files: map[string][]domain.ProcessHandle{activePath: {{PID: 77, Command: "codex"}}}},
	}
	plan, err := PrepareLive(context.Background(), LiveSnapshotRequest{SourceRoot: source, DestinationRoot: destination, ActivityOptions: options})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Assets) != 2 {
		t.Fatalf("unexpected live asset count: %+v", plan.Assets)
	}
	var stable, active LiveAsset
	for _, asset := range plan.Assets {
		switch asset.RelativePath {
		case "sessions/stable.jsonl":
			stable = asset
		case "sessions/active.jsonl":
			active = asset
		}
	}
	if !stable.Complete || stable.Active || active.Complete || !active.Active || active.Queued {
		t.Fatalf("asset-level activity classification is wrong: stable=%+v active=%+v", stable, active)
	}
	result, err := ExecuteLive(context.Background(), plan, ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.CopiedAssets != 2 || result.QueuedAssets != 0 || result.Complete {
		t.Fatalf("unexpected live result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(destination, LiveManifestName)); err != nil {
		t.Fatalf("live manifest missing: %v", err)
	}
	assertOptionalFileUnchanged(t, activePath, activeBefore, activeBeforeInfo)
	for _, relative := range []string{"sessions/stable.jsonl", "sessions/active.jsonl"} {
		if _, err := os.Stat(filepath.Join(destination, relative)); err != nil {
			t.Fatalf("copied live asset missing %s: %v", relative, err)
		}
	}
}

func TestExecuteLiveQueuesRewriteConflictButPublishesStableAssets(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "live-copy")
	stable := filepath.Join(source, "stable.jsonl")
	conflict := filepath.Join(source, "conflict.jsonl")
	writeCopyTestFile(t, stable, `{"type":"stable"}`+"\n")
	writeCopyTestFile(t, conflict, `{"type":"conflict"}`+"\n")
	stableState, err := catalog.ScanLiveJSONL(context.Background(), stable)
	if err != nil {
		t.Fatal(err)
	}
	conflictState, err := catalog.ScanLiveJSONL(context.Background(), conflict)
	if err != nil {
		t.Fatal(err)
	}
	plan := LiveSnapshotPlan{
		Version: 1, Mode: LiveBestEffort, SourceRoot: source, DestinationRoot: destination,
		Assets: []LiveAsset{
			liveAssetFromState("stable.jsonl", domain.AssetKindJSONL, stableState.State),
			{RelativePath: "conflict.jsonl", Kind: domain.AssetKindJSONL, Consistency: string(catalog.LiveRewriteConflict), CapturedSize: conflictState.State.CapturedBytes, SourceSize: conflictState.State.SourceSizeBytes, SHA256: conflictState.State.SHA256},
		},
	}
	result, err := ExecuteLive(context.Background(), plan, ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.CopiedAssets != 1 || result.QueuedAssets != 1 || result.Complete {
		t.Fatalf("unexpected queued result: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(destination, "stable.jsonl")); err != nil {
		t.Fatalf("stable asset was not published: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "conflict.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("conflict asset should be queued, got: %v", err)
	}
}

func TestExecuteLiveMarksAppendAfterPrepareAsActive(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "live-copy")
	path := filepath.Join(source, "rollout.jsonl")
	original := []byte(`{"type":"before"}` + "\n")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.ScanLiveJSONL(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	plan := LiveSnapshotPlan{
		Version: 1, Mode: LiveBestEffort, SourceRoot: source, DestinationRoot: destination,
		Assets: []LiveAsset{liveAssetFromState("rollout.jsonl", domain.AssetKindJSONL, snapshot.State)},
	}
	if err := os.WriteFile(path, append(original, []byte(`{"type":"after"}`+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLive(context.Background(), plan, ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Published || result.CopiedAssets != 1 || result.QueuedAssets != 0 || result.Complete {
		t.Fatalf("追加后的活动资产结果错误：%+v", result)
	}
	if len(result.Assets) != 1 || !result.Assets[0].Active || result.Assets[0].Complete || result.Assets[0].Consistency != "active_append" {
		t.Fatalf("追加后的资产不应继续标记为稳定：%+v", result.Assets)
	}
	copyContent, err := os.ReadFile(filepath.Join(destination, "rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copyContent) != string(original) {
		t.Fatalf("活动副本应只发布准备阶段捕获的前缀：%q", copyContent)
	}
}

func TestExecuteLiveUsesSQLiteOnlineBackupAndLeavesWALUntouched(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "live-copy")
	databasePath := filepath.Join(source, "state.sqlite")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	var journalMode string
	if err := database.QueryRow("PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil || strings.ToLower(journalMode) != "wal" {
		database.Close()
		t.Fatalf("启用 SQLite WAL 失败：mode=%q err=%v", journalMode, err)
	}
	if _, err := database.Exec("PRAGMA wal_autocheckpoint=0; CREATE TABLE messages(id INTEGER PRIMARY KEY, body TEXT);"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	transaction, err := database.Begin()
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if _, err := transaction.Exec("INSERT INTO messages(body) VALUES('first')"); err != nil {
		_ = transaction.Rollback()
		database.Close()
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		database.Close()
		t.Fatal(err)
	}
	reader, err := sql.Open("sqlite", databasePath)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	reader.SetMaxOpenConns(1)
	readTransaction, err := reader.Begin()
	if err != nil {
		reader.Close()
		database.Close()
		t.Fatal(err)
	}
	var existingCount int
	if err := readTransaction.QueryRow("SELECT COUNT(*) FROM messages").Scan(&existingCount); err != nil {
		_ = readTransaction.Rollback()
		reader.Close()
		database.Close()
		t.Fatal(err)
	}
	walPath := databasePath + "-wal"
	shmPath := databasePath + "-shm"
	options := &activity.Options{Interval: time.Millisecond, ProcessDetector: liveFixedProcessDetector{files: map[string][]domain.ProcessHandle{}}}
	plan, err := PrepareLive(context.Background(), LiveSnapshotRequest{SourceRoot: source, DestinationRoot: destination, ActivityOptions: options})
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if len(plan.Assets) != 1 || plan.Assets[0].Kind != domain.AssetKindSQLite || len(plan.ExcludedPaths) != 2 {
		database.Close()
		t.Fatalf("SQLite live plan did not separate WAL/SHM: %+v", plan)
	}
	if _, err := database.Exec("INSERT INTO messages(body) VALUES('second')"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	beforeDatabase, beforeDatabaseInfo := snapshotOptionalFile(t, databasePath)
	beforeWAL, beforeWALInfo := snapshotOptionalFile(t, walPath)
	beforeSHM, beforeSHMInfo := snapshotOptionalFile(t, shmPath)
	result, err := ExecuteLive(context.Background(), plan, ExecuteOptions{})
	if err != nil {
		_ = readTransaction.Rollback()
		reader.Close()
		database.Close()
		t.Fatal(err)
	}
	if !result.Published || result.CopiedAssets != 1 || result.QueuedAssets != 0 || !result.Complete {
		_ = readTransaction.Rollback()
		reader.Close()
		database.Close()
		t.Fatalf("unexpected SQLite live result: %+v", result)
	}
	assertOptionalFileUnchanged(t, databasePath, beforeDatabase, beforeDatabaseInfo)
	assertOptionalFileUnchanged(t, walPath, beforeWAL, beforeWALInfo)
	assertOptionalFileUnchanged(t, shmPath, beforeSHM, beforeSHMInfo)
	_ = readTransaction.Rollback()
	_ = reader.Close()
	_ = database.Close()
	if _, err := os.Stat(filepath.Join(destination, "state.sqlite-wal")); !os.IsNotExist(err) {
		t.Fatalf("活动 SQLite 副本不应复制原始 WAL：%v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "state.sqlite-shm")); !os.IsNotExist(err) {
		t.Fatalf("活动 SQLite 副本不应复制原始 SHM：%v", err)
	}
	destinationDB, err := sql.Open("sqlite", filepath.Join(destination, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer destinationDB.Close()
	var count int
	if err := destinationDB.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("online backup missed committed WAL row: count=%d", count)
	}
	if _, _, err := sqliteguard.LiveReadOnlyDSN(databasePath, 1000); err != nil {
		t.Fatal(err)
	}
}

func snapshotOptionalFile(t *testing.T, path string) ([]byte, os.FileInfo) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 fixture 旁路文件失败 %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("读取 fixture 旁路文件状态失败 %s: %v", path, err)
	}
	return content, info
}

func assertOptionalFileUnchanged(t *testing.T, path string, expected []byte, expectedInfo os.FileInfo) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) || info.Size() != expectedInfo.Size() || !info.ModTime().Equal(expectedInfo.ModTime()) {
		t.Fatalf("源 SQLite 旁路文件被改变 %s: before=%d after=%d", path, len(expected), len(actual))
	}
}

func TestLivePlanDoesNotRequireCodexToBeIdle(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), strings.Repeat(`{"type":"item"}`+"\n", 2))
	plan, err := PrepareLive(context.Background(), LiveSnapshotRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: liveFixedProcessDetector{files: map[string][]domain.ProcessHandle{}}}})
	if err != nil || len(plan.Assets) != 1 {
		t.Fatalf("live plan unexpectedly requires idle source: plan=%+v err=%v", plan, err)
	}
}
