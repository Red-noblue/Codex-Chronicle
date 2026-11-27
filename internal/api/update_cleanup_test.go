// 中文说明：验证更新清理期限设置的参数、持久化和认证边界，保存设置不会直接删除更新文件。
package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-chronicle.local/internal/auth"
	updatepkg "codex-chronicle.local/internal/update"
)

func TestUpdateCleanupRetentionSettings(t *testing.T) {
	s := NewServer(t.TempDir())
	call := func(method, body, contentType string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		request := httptest.NewRequest(method, "/api/v1/update/cleanup-settings", strings.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		s.Handler().ServeHTTP(r, request)
		return r
	}
	if r := call(http.MethodPut, `{"retention_minutes":1}`, "application/json"); r.Code != http.StatusConflict {
		t.Fatal("没有更新管理器时接受了清理设置", r.Code)
	}
	m, err := updatepkg.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetUpdateManager(m)
	for _, body := range []string{`{}`, `null`, `{"retention_minutes":0}`, `{"retention_minutes":-1}`, `{"retention_minutes":43201}`, `{"retention_minutes":1.5}`, `{"retention_minutes":"1"}`, `{"retention_minutes":1} {}`, `{"retention_minutes":1,"unknown":true}`} {
		if r := call(http.MethodPut, body, "application/json"); r.Code != http.StatusBadRequest {
			t.Fatalf("接受了非法设置 %s：%d %s", body, r.Code, r.Body.String())
		}
	}
	if r := call(http.MethodPut, `{"retention_minutes":5}`, "application/json"); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"cleanup_retention_minutes":5`) {
		t.Fatalf("设置保存失败：%d %s", r.Code, r.Body.String())
	}
	if r := call(http.MethodGet, "", "application/json"); r.Code != http.StatusMethodNotAllowed {
		t.Fatal("GET 修改了清理设置")
	}
	if r := call(http.MethodPut, `{"retention_minutes":1}`, "text/plain"); r.Code != http.StatusUnsupportedMediaType {
		t.Fatal("接受了非 JSON 的设置请求")
	}
	security, _, err := auth.NewManager(auth.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s.SetAuthManager(security)
	if r := call(http.MethodPut, `{"retention_minutes":1}`, "application/json"); r.Code != http.StatusUnauthorized {
		t.Fatal("清理期限设置绕过了认证", r.Code)
	}
}
