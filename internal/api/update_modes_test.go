// 中文说明：验证模式API严格校验、旧开关兼容、持久化暂停与私有状态轮询不影响空闲窗口。
package api

import (
	updatepkg "codex-chronicle.local/internal/update"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestUpdateModeAndDownloadControlAPI(t *testing.T) {
	s := NewServer(t.TempDir())
	m, _ := updatepkg.NewManager(t.TempDir())
	s.SetUpdateManager(m)
	s.SetAutomaticUpdateRestart(func(context.Context, string) error { return nil })
	handler := s.Handler()
	call := func(path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if path == "/api/v1/update/automatic" {
			r.Method = http.MethodPut
		}
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, body := range []string{`{"mode":"unknown"}`, `{"mode":"download","enabled":true}`, `{"mode":null}`, `{"mode":"notify","extra":1}`} {
		if w := call("/api/v1/update/automatic", body); w.Code != 400 {
			t.Fatalf("非法模式未拒绝：%d %s", w.Code, w.Body.String())
		}
	}
	for _, mode := range []string{"automatic", "download", "notify", "manual"} {
		if w := call("/api/v1/update/automatic", `{"mode":"`+mode+`"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"`+mode+`"`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, body := range []string{`{}`, `{"action":"bad"}`, `{"action":"pause","extra":true}`} {
		if w := call("/api/v1/update/control", body); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := call("/api/v1/update/control", `{"action":"pause"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"download_paused":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("/api/v1/update/control", `{"action":"resume"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"download_paused":false`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := call("/api/v1/update/control", `{"action":"download"}`); w.Code != 202 || !strings.Contains(w.Body.String(), `"manual_download":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	s.updateLastActivity = time.Now().Add(-updateIdleWindow - time.Second)
	before := s.updateLastActivity
	w := httptest.NewRecorder()
	s.DesktopUpdateHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/update/status", nil))
	if w.Code != 200 || !s.updateLastActivity.Equal(before) || !s.automaticUpdateIdle() {
		t.Fatal("托盘轮询延后了空闲窗口", w.Code)
	}
}
