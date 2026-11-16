// 中文说明：实现不依赖外部模型的确定性中文摘要，并在发送或展示前过滤常见凭证形态。
package summary

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
)

const (
	LocalProviderID = "local-deterministic"
	defaultMaxChars = 1800
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:sk-[a-z0-9]{16,}|gh[pousr]_[a-z0-9_]{20,}|github_pat_[a-z0-9_]{20,})\b`),
	regexp.MustCompile(`(?i)\bBearer\s+[a-z0-9._~+/=-]{16,}`),
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|password|secret)\s*[:=]\s*["']?[^,\s"']{8,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]+ PRIVATE KEY-----[\s\S]*?-----END [A-Z ]+ PRIVATE KEY-----`),
}

// LocalProvider 使用 inventory 元数据、首条用户消息和有限事件窗口生成摘要。
type LocalProvider struct{}

// NewLocalProvider 创建本地确定性 provider。
func NewLocalProvider() LocalProvider {
	return LocalProvider{}
}

// ID 返回稳定 provider 标识。
func (LocalProvider) ID() string {
	return LocalProviderID
}

// Summarize 生成可解释的摘要；它不会声称覆盖未读取的完整正文。
func (p LocalProvider) Summarize(ctx context.Context, request Request) (domain.ConversationSummaryResult, error) {
	if err := ctx.Err(); err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	conversation := request.Conversation
	maxChars := request.MaxChars
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}
	if maxChars > 12000 {
		maxChars = 12000
	}
	redactionCount := 0
	redact := func(value string) string {
		cleaned, count := redactSecrets(value)
		redactionCount += count
		return cleaned
	}

	lines := make([]string, 0, 8)
	if title := redact(conversation.Title); title != "" {
		lines = append(lines, "主题："+title)
	}
	if first := redact(conversation.FirstUserText); first != "" {
		lines = append(lines, "用户目标："+first)
	}
	if cwd := redact(conversation.CWD); cwd != "" {
		lines = append(lines, "工作目录："+cwd)
	}
	modelParts := make([]string, 0, 2)
	if strings.TrimSpace(conversation.ModelProvider) != "" {
		modelParts = append(modelParts, strings.TrimSpace(conversation.ModelProvider))
	}
	if strings.TrimSpace(conversation.Model) != "" {
		modelParts = append(modelParts, strings.TrimSpace(conversation.Model))
	}
	model := strings.Join(modelParts, " / ")
	if model != "" {
		lines = append(lines, "模型："+redact(model))
	}
	lines = append(lines, fmt.Sprintf("规模：%d 条事件，%d 个用户消息，%d 个助手消息，%d 回合，约 %d tokens。",
		conversation.EventCount, conversation.UserMessages, conversation.AssistantMessages, conversation.TurnCount, conversation.TokenUsage.TotalTokens))

	evidence := []domain.SummaryEvidence{{Kind: "inventory", Label: "对话元数据"}}
	if conversation.FirstUserText != "" {
		evidence = append(evidence, domain.SummaryEvidence{Kind: "first_user_message", Label: "首条用户消息"})
	}
	if len(request.Events) > 0 {
		evidence = append(evidence, domain.SummaryEvidence{Kind: "event_window", Label: fmt.Sprintf("前 %d 条可解析事件", len(request.Events))})
		for _, event := range request.Events {
			if err := ctx.Err(); err != nil {
				return domain.ConversationSummaryResult{}, err
			}
			text := strings.TrimSpace(redact(event.Text))
			if text == "" || text == strings.TrimSpace(conversation.FirstUserText) {
				continue
			}
			role := event.Role
			if role == "" {
				role = "事件"
			}
			lines = append(lines, "事件（"+role+"）："+text)
			if len(lines) >= 8 {
				break
			}
		}
	}

	confidence := "low"
	if conversation.FirstUserText != "" {
		confidence = "high"
	} else if len(request.Events) > 0 {
		confidence = "medium"
	}
	limitations := []string{"本地确定性摘要只使用 inventory 元数据和有限事件窗口，不等同于完整正文总结。"}
	if len(request.Events) == 0 {
		limitations = append(limitations, "未读取到可解析事件，主题判断主要依赖元数据。")
	}
	text := strings.Join(lines, "\n")
	text = limitText(text, maxChars)
	return domain.ConversationSummaryResult{
		ConversationID: conversation.ID,
		ProviderID:     p.ID(),
		Text:           text,
		GeneratedAt:    time.Now().UTC(),
		Confidence:     confidence,
		Evidence:       evidence,
		Limitations:    limitations,
		Redacted:       redactionCount > 0,
		RedactionCount: redactionCount,
	}, nil
}

func redactSecrets(value string) (string, int) {
	count := 0
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllStringFunc(value, func(string) string {
			count++
			return "[已脱敏]"
		})
	}
	return value, count
}

// RedactText 对准备发送给外部 provider 或展示给用户的文本执行通用脱敏。
func RedactText(value string) (string, int) {
	return redactSecrets(value)
}

func limitText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return value
	}
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}
