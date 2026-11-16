// 中文说明：验证多版本 JSONL 只读投影、消息摘要、token 统计和活动诊断。
package compat

import (
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestAnalyzeRolloutBuildsConversationProjection(t *testing.T) {
	input := strings.NewReader("{" +
		"\"type\":\"session_meta\",\"payload\":{" +
		"\"id\":\"thread-1\",\"cwd\":\"/work/demo\",\"cli_version\":\"0.164.0\",\"model_provider\":\"openai\"}," +
		"\"timestamp\":\"2026-10-26T10:00:00Z\"}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"整理这个项目的删除策略\"}]},\"timestamp\":\"2026-10-26T10:01:00Z\"}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"token_count\",\"info\":{\"total_token_usage\":{\"input_tokens\":10,\"output_tokens\":4,\"total_tokens\":14}}}}\n")

	projection, scan, err := AnalyzeRollout(input, "sessions/demo.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if scan.LineCount != 3 || projection.SourceID != "thread-1" {
		t.Fatalf("unexpected projection: scan=%+v projection=%+v", scan, projection)
	}
	if projection.ConversationID == "" || projection.CWD != "/work/demo" || projection.CLIVersion != "0.164.0" {
		t.Fatalf("missing session metadata: %+v", projection)
	}
	if projection.UserMessages != 1 || !strings.Contains(projection.FirstUserText, "删除策略") || projection.TokenUsage.TotalTokens != 14 {
		t.Fatalf("missing message/token projection: %+v", projection)
	}
	if projection.Status != domain.StatusStable || projection.Orphan {
		t.Fatalf("unexpected stable status: %+v", projection)
	}
}

func TestAnalyzeRolloutDistinguishesPendingAndCompleteInvalidLine(t *testing.T) {
	projection, scan, err := AnalyzeRollout(strings.NewReader("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-2\"}}\n{\"type\":\""), "rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if scan.PendingLines != 1 || projection.Status != domain.StatusUnknown || len(projection.Diagnostics) == 0 {
		t.Fatalf("expected unknown pending diagnostic: scan=%+v projection=%+v", scan, projection)
	}

	projection, scan, err = AnalyzeRollout(strings.NewReader("{not-json"), "rollout.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if scan.PendingLines != 0 || projection.Status != domain.StatusUnknown {
		t.Fatalf("complete malformed line must be unknown: scan=%+v projection=%+v", scan, projection)
	}
}
