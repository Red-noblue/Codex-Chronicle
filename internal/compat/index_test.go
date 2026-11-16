// 中文说明：验证 session index 只读解析能保留线程标识和原始行号。
package compat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAnalyzeSessionIndexPathProjectsEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_index.jsonl")
	if err := os.WriteFile(path, []byte("{\"id\":\"thread-a\"}\n{\"thread_id\":\"thread-b\"}\n{bad}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary, err := AnalyzeSessionIndexPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.LineCount != 3 || summary.InvalidJSONLines != 1 || len(summary.Entries) != 2 || summary.Entries[1].Line != 2 {
		t.Fatalf("unexpected session index summary: %+v", summary)
	}
}
