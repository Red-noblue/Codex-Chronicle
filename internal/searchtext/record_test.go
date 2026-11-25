// 中文说明：验证 JSON 对象键顺序与正文分片不会改变可搜索文字。
package searchtext

import (
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestRecordUsesDeterministicObjectOrderAndKeepsArrayOrder(t *testing.T) {
	frame := domain.JSONLFrame{ValidJSON: true}
	first := []byte(`{"type":"event_msg","payload":{"b":["甲","乙"],"a":{"d":"后","c":"前"},"encrypted_content":"opaque"}}`)
	second := []byte(`{"payload":{"encrypted_content":"opaque","a":{"c":"前","d":"后"},"b":["甲","乙"]},"type":"event_msg"}`)
	const want = "前 后 甲 乙 event_msg"
	for i := 0; i < 40; i++ {
		if a, b := Record(frame, first), Record(frame, second); a != want || b != want {
			t.Fatalf("提取顺序不稳定：%q / %q", a, b)
		}
	}
	line := []byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"消息分"},{"text":"片边界"}]}}`)
	if got := Record(frame, line); !strings.Contains(got, "消息分片边界") {
		t.Fatalf("阅读正文中的跨片词组未纳入索引：%q", got)
	}
}
