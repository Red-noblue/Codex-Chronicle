// 中文说明：验证 OpenAI-compatible provider 的显式配置、脱敏请求和响应错误边界。
package summary

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestOpenAICompatibleProviderRedactsRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected authorization header: %q", request.Header.Get("Authorization"))
		}
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body.Messages) != 2 || strings.Contains(body.Messages[1].Content, "sk-1234567890abcdefghijklmnop") {
			t.Fatalf("request leaked secret: %+v", body.Messages)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"摘要中不应保留 sk-1234567890abcdefghijklmnop"}}]}`))
	}))
	defer server.Close()
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleConfig{BaseURL: server.URL, APIKey: "test-key", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Summarize(context.Background(), Request{Conversation: domain.ConversationSummary{ID: "conversation-cloud", FirstUserText: "请处理 sk-1234567890abcdefghijklmnop"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Redacted || result.RedactionCount < 2 || strings.Contains(result.Text, "sk-1234567890abcdefghijklmnop") {
		t.Fatalf("cloud summary did not redact response: %+v", result)
	}
}

func TestOpenAICompatibleProviderRequiresExplicitCredentials(t *testing.T) {
	if _, err := NewOpenAICompatibleProvider(OpenAICompatibleConfig{BaseURL: "https://example.com", Model: "model"}); err == nil {
		t.Fatal("missing API key should be rejected")
	}
	if _, err := NewOpenAICompatibleProvider(OpenAICompatibleConfig{BaseURL: "ftp://example.com", APIKey: "key", Model: "model"}); err == nil {
		t.Fatal("unsupported scheme should be rejected")
	}
}
