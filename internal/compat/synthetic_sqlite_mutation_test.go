// 中文说明：验证合成 SQLite control-plane 的快照、事务删除、恢复和冲突保护。
package compat

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
	_ "modernc.org/sqlite"
)

func TestSyntheticSQLiteMutationDeleteRestoreAndConflictProtection(t *testing.T) {
	root, databasePath, asset := createSyntheticSQLiteFixture(t)
	adapter, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	request := MutationRequest{Root: root, ConversationID: "thread-a", AssetSetHash: "fixture-assets-v1", Assets: []MutationAsset{asset}}
	snapshot, err := adapter.Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.AdapterID != SyntheticFixtureDescriptor().ID || snapshot.SnapshotRef == "" || snapshot.SnapshotSHA256 == "" {
		t.Fatalf("unexpected mutation snapshot: %+v", snapshot)
	}

	outcome, err := adapter.Apply(context.Background(), snapshot, MutationDelete)
	if err != nil || !outcome.Verified || outcome.ChangedRows != 3 {
		t.Fatalf("unexpected delete result: %+v err=%v", outcome, err)
	}
	assertSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          false,
		"thread_items:item-a":       false,
		"thread_spawn_edges:edge-a": false,
		"threads:thread-child":      true,
	})

	restored, err := adapter.Restore(context.Background(), snapshot)
	if err != nil || !restored.Verified || restored.ChangedRows != 3 {
		t.Fatalf("unexpected restore result: %+v err=%v", restored, err)
	}
	assertSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          true,
		"thread_items:item-a":       true,
		"thread_spawn_edges:edge-a": true,
		"threads:thread-child":      true,
	})

	if err := os.WriteFile(snapshot.SnapshotRef, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), snapshot, MutationDelete); err == nil || !strings.Contains(err.Error(), "摘要不匹配") {
		t.Fatalf("tampered snapshot was accepted: %v", err)
	}
}

func TestSyntheticSQLiteMutationRejectsChangedRowsBeforeDelete(t *testing.T) {
	root, databasePath, asset := createSyntheticSQLiteFixture(t)
	adapter, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Prepare(context.Background(), MutationRequest{Root: root, ConversationID: "thread-a", Assets: []MutationAsset{asset}})
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE threads SET title='changed' WHERE id='thread-a'"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), snapshot, MutationDelete); err == nil || !strings.Contains(err.Error(), "已变化") {
		t.Fatalf("changed SQLite row was deleted: %v", err)
	}
	assertSQLiteRows(t, databasePath, map[string]bool{"threads:thread-a": true})
}

func TestSyntheticSQLiteMutationDeletesAndRestoresSessionIndexWithSourceID(t *testing.T) {
	root, databasePath, databaseAsset := createSyntheticSQLiteFixture(t)
	indexPath := filepath.Join(root, "session_index.jsonl")
	if err := os.WriteFile(indexPath, []byte("{\"id\":\"thread-a\",\"title\":\"目标\"}\n{\"id\":\"thread-child\",\"title\":\"子线程\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	indexInfo, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	indexAsset := MutationAsset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: indexInfo.Size(), SHA256: hashFileForTest(t, indexPath), Status: domain.StatusStable}
	adapter, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Prepare(context.Background(), MutationRequest{
		Root: root, ConversationID: "conversation-target", SourceID: "thread-a", Assets: []MutationAsset{databaseAsset, indexAsset},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.SourceID != "thread-a" {
		t.Fatalf("source ID was not bound to snapshot: %+v", snapshot)
	}
	deleted, err := adapter.Apply(context.Background(), snapshot, MutationDelete)
	if err != nil || !deleted.Verified || deleted.ChangedRows != 3 || deleted.ChangedFiles != 1 {
		t.Fatalf("unexpected multi-resource delete: %+v err=%v", deleted, err)
	}
	assertSQLiteRows(t, databasePath, map[string]bool{
		"threads:thread-a":          false,
		"thread_items:item-a":       false,
		"thread_spawn_edges:edge-a": false,
		"threads:thread-child":      true,
	})
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("unexpected deleted session index: %q err=%v", data, err)
	}
	restored, err := adapter.Restore(context.Background(), snapshot)
	if err != nil || !restored.Verified || restored.ChangedRows != 3 || restored.ChangedFiles != 1 {
		t.Fatalf("unexpected multi-resource restore: %+v err=%v", restored, err)
	}
	if data, err := os.ReadFile(indexPath); err != nil || string(data) != "{\"id\":\"thread-a\",\"title\":\"目标\"}\n{\"id\":\"thread-child\",\"title\":\"子线程\"}\n" {
		t.Fatalf("unexpected restored session index: %q err=%v", data, err)
	}
}

func TestSyntheticSQLiteMutationRejectsChangedSessionIndex(t *testing.T) {
	root, _, databaseAsset := createSyntheticSQLiteFixture(t)
	indexPath := filepath.Join(root, "session_index.jsonl")
	if err := os.WriteFile(indexPath, []byte("{\"id\":\"thread-a\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	indexAsset := MutationAsset{RelativePath: "session_index.jsonl", Kind: domain.AssetKindSessionIndex, SizeBytes: info.Size(), SHA256: hashFileForTest(t, indexPath), Status: domain.StatusStable}
	adapter, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := adapter.Prepare(context.Background(), MutationRequest{Root: root, ConversationID: "thread-a", Assets: []MutationAsset{databaseAsset, indexAsset}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(indexPath, []byte("{\"id\":\"thread-a\",\"changed\":true}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Apply(context.Background(), snapshot, MutationDelete); err == nil || !strings.Contains(err.Error(), "session index 已变化") {
		t.Fatalf("changed session index was accepted: %v", err)
	}
}

func TestSyntheticSQLiteMutationRejectsCodexRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	if _, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots")); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewSyntheticSQLiteMutation(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = adapter.Prepare(context.Background(), MutationRequest{
		Root: root, ConversationID: "thread-a", Assets: []MutationAsset{{
			RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: 1,
			SHA256: strings.Repeat("a", 64), Status: domain.StatusStable,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), ".codex") {
		t.Fatalf("expected .codex rejection, got %v", err)
	}
}

func createSyntheticSQLiteFixture(t *testing.T) (string, string, MutationAsset) {
	t.Helper()
	root := t.TempDir()
	databasePath := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`
		CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT, rollout_path TEXT);
		CREATE TABLE thread_items(id TEXT PRIMARY KEY, thread_id TEXT, body TEXT);
		CREATE TABLE thread_spawn_edges(id TEXT PRIMARY KEY, parent_thread_id TEXT, child_thread_id TEXT);
		INSERT INTO threads VALUES('thread-a','原始标题','sessions/a.jsonl');
		INSERT INTO threads VALUES('thread-child','子线程','sessions/child.jsonl');
		INSERT INTO thread_items VALUES('item-a','thread-a','正文');
		INSERT INTO thread_items VALUES('item-child','thread-child','子正文');
		INSERT INTO thread_spawn_edges VALUES('edge-a','thread-a','thread-child');
	`)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := hashFileForTest(t, databasePath)
	return root, databasePath, MutationAsset{RelativePath: "state.sqlite", Kind: domain.AssetKindSQLite, SizeBytes: info.Size(), SHA256: digest, Status: domain.StatusStable}
}

func assertSQLiteRows(t *testing.T, databasePath string, expected map[string]bool) {
	t.Helper()
	database, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for key, want := range expected {
		parts := strings.SplitN(key, ":", 2)
		var count int
		if err := database.QueryRow("SELECT COUNT(*) FROM \""+parts[0]+"\" WHERE id=?", parts[1]).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if (count > 0) != want {
			t.Fatalf("unexpected %s presence: got=%v want=%v", key, count > 0, want)
		}
	}
}

func hashFileForTest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
