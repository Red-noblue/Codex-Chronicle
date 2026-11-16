// 中文说明：验证 rollout 事件窗口的角色提取、字节定位、分页和长文本截断。
package compat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRolloutEventsPathPaginatesAndKeepsOffsets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	content := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"thread-events"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"第一个问题"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第一个回答"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"第二个问题"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	items, total, _, err := ReadRolloutEventsPath(context.Background(), path, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(items) != 2 {
		t.Fatalf("unexpected pagination: total=%d items=%+v", total, items)
	}
	if items[0].Role != "assistant" || items[0].Text != "第一个回答" || items[1].Role != "user" || items[1].Line != 4 {
		t.Fatalf("unexpected events: %+v", items)
	}
	if items[0].StartOffset <= 0 || items[0].EndOffset <= items[0].StartOffset {
		t.Fatalf("event offsets missing: %+v", items[0])
	}
}

func TestReadRolloutEventsPathTruncatesLargePreview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	longText := strings.Repeat("长", maxEventPreviewBytes)
	content := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + longText + `"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	items, total, _, err := ReadRolloutEventsPath(context.Background(), path, 0, 1)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("unexpected long event result: total=%d items=%d err=%v", total, len(items), err)
	}
	if !items[0].Truncated || !strings.HasSuffix(items[0].Text, "…") {
		t.Fatalf("expected truncated preview: %+v", items[0])
	}
}
