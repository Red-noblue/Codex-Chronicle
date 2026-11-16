// 中文说明：验证辅助文件不生成幽灵会话、不污染搜索，并保留导入会话、官方归档与损坏 rollout 的可见性。
package application

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestScannerSeparatesConversationsFromCodexSupportFiles(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"sessions/2026/11/12/rollout-real.jsonl":     "{\"type\":\"session_meta\",\"payload\":{\"id\":\"real\"}}\n",
		"archived_sessions/rollout-archived.jsonl":   "{\"type\":\"session_meta\",\"payload\":{\"id\":\"archived\"}}\n",
		"imports/responses.jsonl":                    "{\"type\":\"session_meta\",\"payload\":{\"id\":\"imported\"}}\n",
		"sessions/damaged.jsonl":                     "{\"type\":\"session_meta\"",
		"history.jsonl":                              "{\"session_id\":\"real\",\"ts\":123,\"text\":\"输入历史\"}\n",
		"responses.jsonl":                            "{\"type\":\"response.done\",\"response\":{\"usage\":{\"total_tokens\":42}}}\n",
		"telemetry.jsonl":                            "{\"thread_id\":\"telemetry-only\",\"usage\":{\"total_tokens\":42}}\n",
		"session_index.jsonl":                        "{\"id\":\"real\",\"thread_name\":\"真实会话\"}\n",
		".tmp/plugins/fixtures/rollout-sample.jsonl": "{\"type\":\"session_meta\",\"payload\":{\"id\":\"fixture-only\"}}\n",
		"skills/fixture/session_index.jsonl":         "{\"id\":\"fixture-only\"}\n",
		"logs/rollout-example.jsonl":                 "{\"type\":\"session_meta\",\"payload\":{\"id\":\"log-only\"}}\n",
	}
	for relative, content := range files {
		file := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(root, ".tmp", "state.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE threads(id TEXT PRIMARY KEY, rollout_path TEXT); INSERT INTO threads VALUES('fixture-db-only','rollout-sample.jsonl')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := NewScanner().Scan(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Files) != len(files)+1 || len(report.Errors) != 0 || len(report.Conversations) != 4 {
		t.Fatalf("辅助资产丢失或仍产生伪会话: files=%d errors=%v conversations=%+v", len(report.Files), report.Errors, report.Conversations)
	}
	seen := make(map[string]bool)
	for _, conversation := range report.Conversations {
		seen[conversation.SourceID] = true
	}
	for _, id := range []string{"real", "archived", "imported", filepath.Join(root, "sessions", "damaged.jsonl")} {
		if !seen[id] {
			t.Fatalf("有效导入/归档或损坏对话被误隐藏: %s", id)
		}
	}
	for _, file := range report.Files {
		switch file.RelativePath {
		case "history.jsonl":
			if file.Kind != domain.AssetKindInputHistory || file.Rollout != nil || file.Status != domain.StatusStable {
				t.Fatalf("输入历史仍作为未知对话解析: %+v", file)
			}
		case "responses.jsonl", "telemetry.jsonl", ".tmp/plugins/fixtures/rollout-sample.jsonl", "logs/rollout-example.jsonl":
			if file.Kind != domain.AssetKindGenericJSONL || file.Rollout != nil {
				t.Fatalf("辅助 JSONL 仍被认作 rollout: %+v", file)
			}
		case ".tmp/state.sqlite":
			if file.SQLite != nil {
				t.Fatalf("插件样本数据库仍参与生成会话: %+v", file)
			}
		case "sessions/damaged.jsonl":
			if file.Rollout == nil || len(file.Rollout.Diagnostics) == 0 {
				t.Fatalf("损坏 rollout 的诊断被丢弃: %+v", file)
			}
		}
	}
	for relative, expected := range files {
		actual, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil || string(actual) != expected {
			t.Fatalf("只读扫描改写了源文件 %s: %v", relative, err)
		}
	}
}

func TestSearchSkipsSupportFilesWithoutConsumingResultLimit(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"history.jsonl":                      "{\"session_id\":\"target\",\"ts\":123,\"text\":\"分类关键词\"}\n",
		"responses.jsonl":                    "{\"type\":\"response.done\",\"response\":{\"metadata\":\"分类关键词\"}}\n",
		"session_index.jsonl":                "{\"id\":\"target\",\"thread_name\":\"分类关键词\"}\n",
		".tmp/plugins/rollout-fixture.jsonl": "{\"type\":\"session_meta\",\"payload\":{\"id\":\"fixture\"}}\n{\"text\":\"分类关键词\"}\n",
		"z-import.jsonl":                     "{\"type\":\"session_meta\",\"payload\":{\"id\":\"target\"}}\n{\"type\":\"message\",\"text\":\"分类关键词\"}\n",
	}
	paths := make([]string, 0, len(files))
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, relative)
	}
	for _, live := range []bool{false, true} {
		var hits []domain.SearchHit
		var err error
		if live {
			hits, err = SearchJSONL(t.Context(), root, "分类关键词", 1)
		} else {
			hits, err = SearchJSONLFiles(t.Context(), root, paths, "分类关键词", 1)
		}
		if err != nil || len(hits) != 1 || hits[0].RelativePath != "z-import.jsonl" {
			t.Fatalf("辅助记录挤占了会话搜索结果: live=%v hits=%+v err=%v", live, hits, err)
		}
	}
}
