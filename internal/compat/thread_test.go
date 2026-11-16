// 中文说明：验证 Codex threads 表元数据和线程关系以只读方式提取。
package compat

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAnalyzeThreadDatabasePathSeesCommittedWALChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state_5.sqlite")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT);
		CREATE TABLE thread_items(thread_id TEXT, item_json TEXT);
		INSERT INTO threads VALUES('deleted','旧对话'),('protected','保留对话');
		INSERT INTO thread_items VALUES('deleted','{}');
		PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;`); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(`DELETE FROM thread_items WHERE thread_id='deleted';
		DELETE FROM threads WHERE id='deleted';
		UPDATE threads SET title='最新标题' WHERE id='protected';
		INSERT INTO threads VALUES('new','新对话');`); err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal"} {
		before[suffix], err = os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
	}
	threads, relations, err := AnalyzeThreadDatabasePath(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]string)
	for _, thread := range threads {
		byID[thread.ID] = thread.Title
	}
	if len(byID) != 2 || byID["protected"] != "最新标题" || byID["new"] != "新对话" {
		t.Fatalf("读取了忽略 WAL 的旧线程记录: %+v", threads)
	}
	if len(relations) != 0 {
		t.Fatalf("已删除线程的关系在 WAL 中被清除后仍被返回: %+v", relations)
	}
	for suffix, content := range before {
		after, err := os.ReadFile(path + suffix)
		if err != nil || string(content) != string(after) {
			t.Fatalf("只读分析改写了数据库或 WAL %s: %v", suffix, err)
		}
	}
	// 缺少 SHM 的副本不能尝试重建源旁路文件，也不能降级读取旧主库。
	copyPath := filepath.Join(t.TempDir(), "incomplete.sqlite")
	for suffix, content := range before {
		if err := os.WriteFile(copyPath+suffix, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := AnalyzeThreadDatabasePath(t.Context(), copyPath); err == nil {
		t.Fatal("缺少 SHM 的活动数据库没有拒绝读取")
	}
	if _, err := os.Stat(copyPath + "-shm"); !os.IsNotExist(err) {
		t.Fatalf("只读分析创建了缺失的 SHM: %v", err)
	}
}

func TestAnalyzeThreadDatabasePathProjectsRelationships(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state_5.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`
		CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT, title TEXT, first_user_message TEXT, cwd TEXT, model TEXT, model_provider TEXT, cli_version TEXT, created_at TEXT, updated_at TEXT, tokens_used INTEGER, archived INTEGER);
		CREATE TABLE thread_attachments(id TEXT PRIMARY KEY, thread_id TEXT, payload TEXT);
		CREATE TABLE thread_spawn_edges(parent_thread_id TEXT, child_thread_id TEXT, status TEXT);
		INSERT INTO threads VALUES('thread-a','sessions/a.jsonl','标题','第一条','/work','gpt','openai','0.1','2026-01-01T00:00:00Z','2026-01-01T00:01:00Z',42,0);
		INSERT INTO thread_attachments VALUES('attachment-a','thread-a','{}');
		INSERT INTO thread_spawn_edges VALUES('thread-a','thread-child','open');
		INSERT INTO thread_spawn_edges VALUES('thread-a','thread-child-2','open');
	`)
	if err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	threads, relationships, err := AnalyzeThreadDatabasePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "thread-a" || threads[0].TokensUsed != 42 || threads[0].Title != "标题" {
		t.Fatalf("unexpected thread projection: %+v", threads)
	}
	if len(relationships) != 2 {
		t.Fatalf("unexpected relationships: %+v", relationships)
	}
	var attachmentRelation bool
	var spawnRelation bool
	for _, relationship := range relationships {
		if relationship.Table == "thread_attachments" && relationship.Relation == "attachment" {
			attachmentRelation = true
			if len(relationship.RowKeys) != 1 || relationship.RowKeys[0].Key != "attachment-a" || !relationship.RowsComplete {
				t.Fatalf("attachment row identity was not projected: %+v", relationship)
			}
		}
		if relationship.Table == "thread_spawn_edges" && relationship.Relation == "parent_thread" {
			spawnRelation = true
			if relationship.RelatedThreadID != "thread-child" || len(relationship.RelatedThreadIDs) != 2 || relationship.RelatedThreadIDs[1] != "thread-child-2" || len(relationship.RowKeys) != 2 || relationship.RowKeys[0].KeyKind != "rowid" || !relationship.RowsComplete {
				t.Fatalf("spawn edge row identity was not projected: %+v", relationship)
			}
		}
	}
	if !attachmentRelation {
		t.Fatalf("attachment relationship was not classified: %+v", relationships)
	}
	if !spawnRelation {
		t.Fatalf("spawn relationship was not classified: %+v", relationships)
	}
}

func TestAnalyzeThreadDatabasePathWithoutThreadsIsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE thread_items(thread_id TEXT, item_json TEXT)"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	database.Close()
	threads, relationships, err := AnalyzeThreadDatabasePath(context.Background(), path)
	if err != nil || threads != nil || relationships != nil {
		t.Fatalf("expected empty projection: threads=%v relationships=%v err=%v", threads, relationships, err)
	}
}

func TestAnalyzeThreadDatabasePathReadsUnboundThreadReferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE thread_items(thread_id TEXT, item_json TEXT); INSERT INTO thread_items VALUES('thread-a', '{}');"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	threads, relationships, err := AnalyzeThreadDatabasePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if threads != nil || len(relationships) != 1 || relationships[0].ThreadID != "thread-a" || relationships[0].RowCount != 1 {
		t.Fatalf("unexpected unbound relationship projection: threads=%v relationships=%+v", threads, relationships)
	}
}
