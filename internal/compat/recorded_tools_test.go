// 中文说明：验证新版事件、数组工具输出与结构化问答；失败记录和调用意图不得被认作成功。
package compat

import (
	"codex-chronicle.local/internal/domain"
	"encoding/json"
	"strings"
	"testing"
)

func parseRecorded(t *testing.T, data string) domain.ConversationEvent {
	t.Helper()
	e, ok := ParseFullReaderEvent(domain.JSONLFrame{ValidJSON: true}, []byte(data))
	if !ok {
		t.Fatal("event not parsed", data)
	}
	return e
}
func TestRecordedNewItemsAndArrayOutput(t *testing.T) {
	user := parseRecorded(t, `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u","content":[{"type":"Text","text":"问题"}]}}}`)
	if user.Role != "user" || user.Text != "问题" || user.ItemID != "u" {
		t.Fatal(user)
	}
	command := parseRecorded(t, `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"cmd","command":["sh","-c","printf test"],"cwd":"/project","status":"completed","exit_code":2,"aggregated_output":"error"}}}`)
	if command.Status != "failed" || command.Tool.Target != "sh -c printf test" || command.Tool.Output != "error" || command.Tool.WorkingDirectory != "/project" {
		t.Fatal(command)
	}
	output := parseRecorded(t, `{"type":"response_item","payload":{"type":"function_call_output","call_id":"x","output":[{"type":"input_text","text":"hello"},{"type":"input_text","text":" world"}]}}`)
	if output.Text != "hello world" || output.Status != "unconfirmed" {
		t.Fatal(output)
	}
}
func TestRecordedFileChangeAndQuestionReply(t *testing.T) {
	e := parseRecorded(t, `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"f","status":"completed","changes":[{"path":"src/old.go","kind":"update","move_path":"src/new.go","diff":"@@\n-a\n+b\n+c"}]}}}`)
	if len(e.Changes) != 1 || e.Changes[0].Kind != "rename" || e.Changes[0].Added != 2 || e.Changes[0].Removed != 1 || e.Changes[0].Status != "completed" {
		t.Fatal(e.Changes)
	}
	text := `<send_user_message_question_reply>[{"question":"选择什么？","answer":"本地","questionItemId":"id"}]</send_user_message_question_reply>`
	b, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": text}})
	e = parseRecorded(t, string(b))
	if e.Kind != "question_reply" || e.Text != "选择什么？\n本地" {
		t.Fatal(e)
	}
	b, _ = json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "引用示例 " + text}})
	e = parseRecorded(t, string(b))
	if e.Kind == "question_reply" || !strings.HasPrefix(e.Text, "引用示例") {
		t.Fatal("quoted protocol misclassified", e)
	}
}
