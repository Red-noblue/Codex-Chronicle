// 中文说明：验证搜索支持 JSON 字符串中的转义换行，并保留原始行定位。
package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSearchJSONLReturnsByteRangeAndConversation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"search-thread\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"第一行\\n第二行\"}]}}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONL(context.Background(), root, "第一行\n第二行", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 || hits[0].StartOffset <= 0 || hits[0].EndOffset <= hits[0].StartOffset {
		t.Fatalf("unexpected search hits: %+v", hits)
	}
	if hits[0].ConversationID == "" || hits[0].Snippet == "" {
		t.Fatalf("search hit lacks evidence: %+v", hits[0])
	}
}

func TestSearchJSONLFilesUsesDiscoveredPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "first.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"first\"}}\n{\"type\":\"message\",\"text\":\"目标关键词\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.jsonl"), []byte("{\"text\":\"目标关键词\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONLFiles(context.Background(), root, []string{"first.jsonl"}, "目标关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].RelativePath != "first.jsonl" || hits[0].ConversationID == "" {
		t.Fatalf("按已发现路径搜索结果错误: %+v", hits)
	}
}

func TestSearchJSONLFilesSkipsPendingTailAndReportsLiveBoundary(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "active.jsonl")
	content := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"active-thread\"}}\n" +
		"{\"type\":\"message\",\"text\":\"已完成关键词\"}\n" +
		"{\"type\":\"message\",\"text\":\"未完成关键词")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	hits, err := SearchJSONLFiles(context.Background(), root, []string{"active.jsonl"}, "关键词", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Line != 2 || hits[0].SourceStatus != "active_writing" || hits[0].ActivityStatus != "pending_jsonl_line" || hits[0].LiveConsistency != "active_pending_line" || !hits[0].ReadOnly {
		t.Fatalf("pending tail leaked into search or live evidence missing: %+v", hits)
	}
}
