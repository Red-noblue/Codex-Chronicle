// 中文说明：验证真实结构副本 adapter 的标记边界、关系删除、恢复和冲突拒绝。
package compat

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestRealShapeCopyMutationDeleteRestore(t *testing.T) {
	root := createRealShapeCopyFixtureForTest(t)
	adapter, err := NewRealShapeCopyMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	request := realShapeCopyRequest(t, root)
	snapshot, err := adapter.Prepare(context.Background(), request)
	if err != nil {
		t.Fatalf("准备真实副本快照失败: %v", err)
	}
	if snapshot.AdapterID != RealShapeCopyKind || len(snapshot.Assets) < len(realShapeDatabases)+3 {
		t.Fatalf("真实副本快照资产不完整: %+v", snapshot)
	}
	deleted, err := adapter.Apply(context.Background(), snapshot, MutationDelete)
	if err != nil || !deleted.Verified || deleted.ChangedRows == 0 || deleted.ChangedFiles != 1 {
		t.Fatalf("真实副本删除结果异常: %+v err=%v", deleted, err)
	}
	if _, err := InspectRealShapeCopy(context.Background(), root, realShapeFixtureThreadID); err == nil {
		t.Fatal("删除后目标 thread 不应仍能通过完整副本快照")
	}
	restored, err := adapter.Restore(context.Background(), snapshot)
	if err != nil || !restored.Verified || restored.ChangedRows != deleted.ChangedRows || restored.ChangedFiles != 1 {
		t.Fatalf("真实副本恢复结果异常: %+v err=%v", restored, err)
	}
	if _, err := InspectRealShapeCopy(context.Background(), root, realShapeFixtureThreadID); err != nil {
		t.Fatalf("恢复后真实副本快照不可读: %v", err)
	}
}

func TestRealShapeCopyMutationRejectsUnmarkedAndCodexRoots(t *testing.T) {
	root := createRealShapeFixtureForTest(t, realShapeFixtureScript(t))
	request := realShapeCopyRequest(t, root)
	adapter, err := NewRealShapeCopyMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Prepare(context.Background(), request); err == nil || !strings.Contains(err.Error(), "标记") {
		t.Fatalf("未标记根目录被错误接受: %v", err)
	}
	codexRoot := filepath.Join(t.TempDir(), ".codex")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	request.Root = codexRoot
	if _, err := adapter.Prepare(context.Background(), request); err == nil || !strings.Contains(err.Error(), ".codex") {
		t.Fatalf(".codex 根目录被错误接受: %v", err)
	}
}

func TestRealShapeCopyMutationRejectsTamperedMarkerBeforeDelete(t *testing.T) {
	root := createRealShapeCopyFixtureForTest(t)
	adapter, err := NewRealShapeCopyMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Prepare(context.Background(), realShapeCopyRequest(t, root))
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state_5.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE threads SET title='changed' WHERE id=?", realShapeFixtureThreadID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), snapshot, MutationDelete); err == nil || !strings.Contains(err.Error(), "变化") {
		t.Fatalf("SQLite 行变化后仍执行了删除: %v", err)
	}
	indexData, err := os.ReadFile(filepath.Join(root, realShapeSessionIndex))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexData), realShapeFixtureThreadID) {
		t.Fatal("SQLite 行预检失败后不应先修改 session index")
	}
	if err := os.WriteFile(filepath.Join(root, realShapeCopyMarker), []byte(`{"kind":"tampered"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), snapshot, MutationDelete); err == nil {
		t.Fatal("副本标记被篡改后仍执行了删除")
	}
}

func createRealShapeCopyFixtureForTest(t *testing.T) string {
	t.Helper()
	root := createRealShapeFixtureForTest(t, realShapeFixtureScript(t))
	marker, err := json.Marshal(RealShapeFixtureManifest{Kind: RealShapeCopyKind, SchemaSHA256: RealShapeFixtureSchemaSHA256, SQLiteSemanticsSHA256: RealShapeSQLiteSemanticsSHA256})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, realShapeCopyMarker), append(marker, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func realShapeCopyRequest(t *testing.T, root string) MutationRequest {
	t.Helper()
	path := filepath.Join(root, "state_5.sqlite")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return MutationRequest{
		Root: root, ConversationID: "conversation-real-copy", SourceID: realShapeFixtureThreadID, AssetSetHash: "real-copy-assets-v1",
		Assets: []MutationAsset{{RelativePath: "state_5.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: info.Size(), SHA256: hashFileForTest(t, path), Status: domain.StatusStable}},
	}
}
