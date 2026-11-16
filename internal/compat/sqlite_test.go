// 中文说明：验证 SQLite 结构摘要使用只读连接并生成稳定 schema 指纹。
package compat

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestAnalyzeSQLitePathIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version = 7; CREATE TABLE threads(id TEXT PRIMARY KEY, cwd TEXT NOT NULL); CREATE INDEX threads_cwd_index ON threads(cwd); CREATE TRIGGER threads_noop AFTER INSERT ON threads BEGIN SELECT NEW.id; END;"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := AnalyzeSQLitePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !summary.ReadOnly || summary.UserVersion != 7 || len(summary.Tables) != 1 || summary.SchemaFingerprint == "" {
		t.Fatalf("unexpected sqlite summary: %+v", summary)
	}
	if summary.Tables[0].Name != "threads" || len(summary.Tables[0].Columns) != 2 {
		t.Fatalf("unexpected table summary: %+v", summary.Tables)
	}
	if len(summary.SchemaObjects) != 2 {
		t.Fatalf("schema object summary missing index or trigger: %+v", summary.SchemaObjects)
	}
	firstFingerprint := summary.SchemaFingerprint
	database, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE INDEX threads_id_index ON threads(id)"); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := AnalyzeSQLitePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SchemaFingerprint == firstFingerprint || len(updated.SchemaObjects) != 3 {
		t.Fatalf("schema object change did not alter fingerprint: before=%s after=%s objects=%+v", firstFingerprint, updated.SchemaFingerprint, updated.SchemaObjects)
	}
}

func TestAnalyzeSQLitePathRejectsInvalidFileWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.sqlite")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeSQLitePath(context.Background(), path); err == nil {
		t.Fatal("expected invalid sqlite error")
	}
}

func TestAnalyzeSQLitePathDoesNotModifyExistingWALAndSHM(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY); INSERT INTO threads VALUES('thread-a');"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	auxiliary := map[string][]byte{
		path + "-wal": []byte("stable wal bytes"),
		path + "-shm": []byte("stable shm bytes"),
	}
	type fileSnapshot struct {
		bytes []byte
		mode  os.FileMode
		size  int64
		mtime int64
	}
	snapshots := make(map[string]fileSnapshot, len(auxiliary))
	for file, content := range auxiliary {
		if err := os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		snapshots[file] = fileSnapshot{bytes: append([]byte(nil), content...), mode: info.Mode(), size: info.Size(), mtime: info.ModTime().UnixNano()}
	}

	summary, err := AnalyzeSQLitePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Diagnostic == nil || summary.Diagnostic.Code != "SQLITE_IMMUTABLE_WAL_VIEW" || !strings.Contains(summary.Diagnostic.Message, "WAL/SHM") {
		t.Fatalf("WAL/SHM immutable diagnostic missing: %+v", summary.Diagnostic)
	}
	for file, expected := range snapshots {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != string(expected.bytes) || info.Mode() != expected.mode || info.Size() != expected.size || info.ModTime().UnixNano() != expected.mtime {
			t.Fatalf("SQLite 只读分析修改了旁路文件 %s: before=%+v after_bytes=%q after_info=%+v", file, expected, content, info)
		}
	}
}

func TestAnalyzeSQLitePathSupportsEncodedLocalPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "含空格#目录")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state # 中文.sqlite")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY)"); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	summary, err := AnalyzeSQLitePath(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Tables) != 1 || summary.Tables[0].Name != "threads" {
		t.Fatalf("encoded local path was not analyzed: %+v", summary)
	}
}
