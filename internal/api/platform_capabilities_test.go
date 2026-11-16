// 中文说明：验证平台能力 API 只返回只读探针结果，并保持能力状态可供 UI 消费。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	platformpkg "codex-chronicle.local/internal/platform"
)

func TestPlatformCapabilitiesEndpointIsReadOnly(t *testing.T) {
	server := NewServer("")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/platform/capabilities", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("平台能力读取失败：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ReadOnly       bool                         `json:"read_only"`
		SourceReadOnly bool                         `json:"source_read_only"`
		Platform       platformpkg.CapabilityReport `json:"platform"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("平台能力响应不是有效 JSON：%v", err)
	}
	if !response.ReadOnly || !response.SourceReadOnly || !response.Platform.ReadOnly || len(response.Platform.Capabilities) == 0 {
		t.Fatalf("平台能力响应越过只读边界或为空：%+v", response)
	}
	if response.Platform.OS == "" || response.Platform.Architecture == "" {
		t.Fatalf("平台能力响应缺少运行时身份：%+v", response.Platform)
	}
}

func TestPlatformCapabilitiesEndpointRejectsMutationMethod(t *testing.T) {
	server := NewServer("")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/platform/capabilities", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("平台能力接口接受了非只读方法：status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
