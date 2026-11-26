// 中文说明：验证自动更新配置 API、可见页面心跳、空闲门禁和关闭服务时的调度退出。
package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	updatepkg "codex-chronicle.local/internal/update"
)

func TestAutomaticUpdateSettingsAndVisiblePageBlockRestart(t *testing.T) {
	s := NewServer(t.TempDir())
	m, err := updatepkg.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetUpdateManager(m)
	s.SetAutomaticUpdateRestart(func(context.Context, string) error { return nil })
	handler := s.Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(r, request)
		return r
	}
	for _, body := range []string{`{}`, `{"enabled":"true"}`, `{"enabled":false} {}`, `{"enabled":true,"unknown":1}`} {
		if r := call(http.MethodPut, "/api/v1/update/automatic", body); r.Code != 400 {
			t.Fatalf("接受非法设置：%s", r.Body.String())
		}
	}
	if r := call(http.MethodPut, "/api/v1/update/automatic", `{"enabled":false}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"enabled":false`) {
		t.Fatalf("设置失败：%s", r.Body.String())
	}
	if r := call(http.MethodGet, "/api/v1/update/automatic", ""); r.Code != 405 {
		t.Fatal("GET 改变了更新设置")
	}
	s.updateLastActivity = time.Now().Add(-updateIdleWindow - time.Second)
	if !s.automaticUpdateIdle() {
		t.Fatal("无请求和任务时未空闲")
	}
	if r := call(http.MethodPost, "/api/v1/update/activity", `{}`); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if s.automaticUpdateIdle() {
		t.Fatal("阅读中的可见网页未阻止自动重启")
	}
	s.updateLastActivity = time.Now().Add(-updateIdleWindow - time.Second)
	s.activeRequests = 1
	if s.automaticUpdateIdle() {
		t.Fatal("未完成请求仍允许重启")
	}
	s.activeRequests, s.draining = 0, true
	if s.automaticUpdateIdle() {
		t.Fatal("退出中的服务仍启动更新")
	}
}

func TestAutomaticUpdatesStopWithServiceContext(t *testing.T) {
	s := NewServer(t.TempDir())
	m, err := updatepkg.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.SetUpdateManager(m)
	s.SetAutomaticUpdateRestart(func(context.Context, string) error { t.Error("退出后仍安排更新"); return nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { s.RunAutomaticUpdates(ctx, func() bool { return false }); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("自动更新未随服务退出")
	}
}
