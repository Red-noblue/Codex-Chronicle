// 中文说明：提供显式配置的 OpenAI-compatible 摘要 provider；默认注册表不会启用网络调用。
package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"codex-chronicle.local/internal/domain"
)

const OpenAICompatibleProviderID = "openai-compatible"

// OpenAICompatibleConfig 是云端摘要 provider 的显式配置；凭证只保存在进程内。
type OpenAICompatibleConfig struct {
	BaseURL         string
	APIKey          string
	Model           string
	ProviderID      string
	ProxyURL        string
	MaxOutputTokens int
	HTTPClient      *http.Client
}

// OpenAICompatibleProvider 调用 Chat Completions 兼容接口生成摘要。
type OpenAICompatibleProvider struct {
	id         string
	endpoint   string
	apiKey     string
	model      string
	maxTokens  int
	httpClient *http.Client
}

// NewOpenAICompatibleProvider 创建云端 provider；缺少显式凭证时直接拒绝。
func NewOpenAICompatibleProvider(config OpenAICompatibleConfig) (*OpenAICompatibleProvider, error) {
	base := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("云端摘要 BaseURL 必须是 HTTP/HTTPS 地址")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("云端摘要必须显式提供 API key")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, fmt.Errorf("云端摘要必须显式提供模型名")
	}
	endpoint := base
	if !strings.HasSuffix(endpoint, "/chat/completions") {
		endpoint += "/chat/completions"
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second}
		if proxy := strings.TrimSpace(config.ProxyURL); proxy != "" {
			proxyURL, parseErr := url.Parse(proxy)
			if parseErr != nil || proxyURL.Host == "" || (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") {
				return nil, fmt.Errorf("云端摘要代理必须是 HTTP/HTTPS 地址")
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxyURL)
			client.Transport = transport
		}
	}
	id := strings.TrimSpace(config.ProviderID)
	if id == "" {
		id = OpenAICompatibleProviderID
	}
	maxTokens := config.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = 700
	}
	if maxTokens > 4000 {
		maxTokens = 4000
	}
	return &OpenAICompatibleProvider{id: id, endpoint: endpoint, apiKey: config.APIKey, model: config.Model, maxTokens: maxTokens, httpClient: client}, nil
}

// ID 返回 provider 标识。
func (p *OpenAICompatibleProvider) ID() string {
	if p == nil {
		return ""
	}
	return p.id
}

type chatCompletionRequest struct {
	Model       string        `json:"model"`
	Temperature float64       `json:"temperature"`
	MaxTokens   int           `json:"max_tokens"`
	Messages    []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Summarize 只发送已脱敏的元数据和有限事件窗口，并对模型输出再次脱敏。
func (p *OpenAICompatibleProvider) Summarize(ctx context.Context, request Request) (domain.ConversationSummaryResult, error) {
	if p == nil || p.httpClient == nil || p.endpoint == "" {
		return domain.ConversationSummaryResult{}, fmt.Errorf("云端摘要 provider 未初始化")
	}
	if err := ctx.Err(); err != nil {
		return domain.ConversationSummaryResult{}, err
	}
	prompt, inputRedactions := cloudPrompt(request)
	body, err := json.Marshal(chatCompletionRequest{
		Model: p.model, Temperature: 0, MaxTokens: p.maxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: "你是本地对话管理工具的摘要器。使用简体中文，忠实区分已知事实和推断，不要输出凭证、密钥或完整对话正文。只返回一段适合人工审核的简短摘要。"},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("编码云端摘要请求失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("创建云端摘要请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := p.httpClient.Do(req)
	if err != nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("云端摘要请求失败: %w", err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("读取云端摘要响应失败: %w", err)
	}
	var decoded chatCompletionResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return domain.ConversationSummaryResult{}, fmt.Errorf("解析云端摘要响应失败: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := response.Status
		if decoded.Error != nil && strings.TrimSpace(decoded.Error.Message) != "" {
			message = decoded.Error.Message
		}
		return domain.ConversationSummaryResult{}, fmt.Errorf("云端摘要返回 HTTP %d: %s", response.StatusCode, message)
	}
	if len(decoded.Choices) == 0 || strings.TrimSpace(decoded.Choices[0].Message.Content) == "" {
		return domain.ConversationSummaryResult{}, fmt.Errorf("云端摘要响应没有可用内容")
	}
	text, outputRedactions := RedactText(strings.TrimSpace(decoded.Choices[0].Message.Content))
	return domain.ConversationSummaryResult{
		ConversationID: request.Conversation.ID,
		ProviderID:     p.ID(),
		Text:           text,
		GeneratedAt:    time.Now().UTC(),
		Confidence:     "medium",
		Evidence:       cloudEvidence(request),
		Limitations: []string{
			"摘要由已脱敏的有限元数据和事件窗口发送给云端模型生成，不能替代完整正文。",
			"模型输出可能包含推断，删除决策仍需人工核对原始事件。",
		},
		Redacted:       inputRedactions+outputRedactions > 0,
		RedactionCount: inputRedactions + outputRedactions,
	}, nil
}

func cloudPrompt(request Request) (string, int) {
	conversation := request.Conversation
	modelParts := make([]string, 0, 2)
	if strings.TrimSpace(conversation.ModelProvider) != "" {
		modelParts = append(modelParts, strings.TrimSpace(conversation.ModelProvider))
	}
	if strings.TrimSpace(conversation.Model) != "" {
		modelParts = append(modelParts, strings.TrimSpace(conversation.Model))
	}
	parts := []string{
		"对话 ID：" + conversation.ID,
		"标题：" + conversation.Title,
		"首条用户消息：" + conversation.FirstUserText,
		"工作目录：" + conversation.CWD,
		"模型：" + strings.Join(modelParts, " / "),
		fmt.Sprintf("统计：事件 %d，用户消息 %d，助手消息 %d，回合 %d，tokens %d", conversation.EventCount, conversation.UserMessages, conversation.AssistantMessages, conversation.TurnCount, conversation.TokenUsage.TotalTokens),
	}
	for index, event := range request.Events {
		parts = append(parts, fmt.Sprintf("事件 %d（%s）：%s", index+1, event.Role, event.Text))
	}
	redactions := 0
	for index := range parts {
		var count int
		parts[index], count = RedactText(parts[index])
		redactions += count
	}
	text := strings.Join(parts, "\n")
	if request.MaxChars > 0 {
		text = limitText(text, request.MaxChars)
	}
	return text, redactions
}

func cloudEvidence(request Request) []domain.SummaryEvidence {
	evidence := []domain.SummaryEvidence{{Kind: "inventory", Label: "对话元数据"}}
	if request.Conversation.FirstUserText != "" {
		evidence = append(evidence, domain.SummaryEvidence{Kind: "first_user_message", Label: "首条用户消息"})
	}
	if len(request.Events) > 0 {
		evidence = append(evidence, domain.SummaryEvidence{Kind: "event_window", Label: fmt.Sprintf("前 %d 条可解析事件", len(request.Events))})
	}
	return evidence
}
