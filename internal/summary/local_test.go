// 中文说明：验证本地摘要的确定性边界、事件窗口和敏感字段脱敏行为。
package summary

import (
	"context"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestLocalProviderSummarizesMetadataAndRedactsSecrets(t *testing.T) {
	provider := NewLocalProvider()
	result, err := provider.Summarize(context.Background(), Request{
		Conversation: domain.ConversationSummary{
			ID:                "conversation-test",
			Title:             "整理发布计划",
			FirstUserText:     "请分析项目并使用 sk-1234567890abcdefghijklmnop",
			CWD:               "/workspace/project",
			Model:             "gpt-test",
			EventCount:        4,
			UserMessages:      2,
			AssistantMessages: 2,
			TurnCount:         2,
			TokenUsage:        domain.TokenUsage{TotalTokens: 1234},
		},
		Events: []domain.ConversationEvent{{Role: "assistant", Text: "已完成初步分析，token=secret-value-123456"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderID != LocalProviderID || result.Confidence != "high" {
		t.Fatalf("unexpected provider metadata: %+v", result)
	}
	if !result.Redacted || result.RedactionCount < 2 {
		t.Fatalf("expected secret redaction: %+v", result)
	}
	if strings.Contains(result.Text, "sk-1234567890abcdefghijklmnop") || strings.Contains(result.Text, "secret-value-123456") {
		t.Fatalf("summary leaked secret: %s", result.Text)
	}
	if len(result.Limitations) == 0 || len(result.Evidence) < 2 {
		t.Fatalf("summary boundary metadata missing: %+v", result)
	}
}

func TestRegistryRejectsUnconfiguredProvider(t *testing.T) {
	if _, err := DefaultRegistry().Resolve("cloud"); err == nil {
		t.Fatal("unconfigured provider should be rejected")
	}
}
