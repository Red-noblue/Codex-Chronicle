// 中文说明：验证更新重启能力探测、旧运行版本与待生效版本的区分，以及重启失败不掩盖已完成的版本准备。
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	updatepkg "codex-chronicle.local/internal/update"
)

func TestUpdateRestartCapabilityAndPartialSuccess(t *testing.T) {
	m, err := updatepkg.NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInstallation(t.Context(), makeAPIUpdateBundle(t, "1.0.0", "旧版本"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	plan, err := m.Prepare(t.Context(), makeAPIUpdateBundle(t, "2.0.0", "新版本"), false)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(t.TempDir())
	s.SetVersion("1.0.0")
	s.SetUpdateManager(m)
	handler := s.Handler()
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(r, request)
		return r
	}
	body, _ := json.Marshal(map[string]any{"plan_id": plan.ID, "restart": true})
	r := call(http.MethodPost, "/api/v1/update/activate", string(body))
	if r.Code != http.StatusConflict {
		t.Fatalf("不支持重启的服务仍执行了组合更新：%s", r.Body.String())
	}
	status, _ := m.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" {
		t.Fatal("能力检查失败却已切换版本")
	}
	s.SetUpdateRestart(func(context.Context, string) error { return errors.New("协作进程未能启动") })
	r = call(http.MethodPost, "/api/v1/update/activate", string(body))
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "restart_error") || strings.Contains(r.Body.String(), `"restart_scheduled":true`) {
		t.Fatalf("未正确报告部分成功：%s", r.Body.String())
	}
	r = call(http.MethodGet, "/api/v1/update/status", "")
	for _, want := range []string{`"running_version":"1.0.0"`, `"active_version":"2.0.0"`, `"restart_required":true`, `"restart_available":true`} {
		if !strings.Contains(r.Body.String(), want) {
			t.Fatalf("运行状态未区分待生效版本：%s", r.Body.String())
		}
	}
	calls := 0
	s.SetUpdateRestart(func(_ context.Context, version string) error {
		calls++
		if version != "2.0.0" {
			t.Fatalf("重启目标错误：%s", version)
		}
		return nil
	})
	for _, body := range []string{`{}`, `{"version":"过期的版本"}`, `{"version":"2.0.0"} {}`} {
		r = call(http.MethodPost, "/api/v1/update/restart", body)
		if r.Code < 400 || calls != 0 {
			t.Fatal("缺失、过期或混入尾随数据的确认触发了重启")
		}
	}
	form := httptest.NewRecorder()
	handler.ServeHTTP(form, httptest.NewRequest(http.MethodPost, "/api/v1/update/restart", strings.NewReader(`{"version":"2.0.0"}`)))
	if form.Code != http.StatusUnsupportedMediaType || calls != 0 {
		t.Fatal("普通网页表单可直接触发重启")
	}
	r = call(http.MethodPost, "/api/v1/update/restart", `{"version":"2.0.0"}`)
	if r.Code != http.StatusAccepted || calls != 1 {
		t.Fatalf("未安排待生效版本重启：%s", r.Body.String())
	}
	r = call(http.MethodGet, "/api/v1/update/restart", "")
	if r.Code != http.StatusMethodNotAllowed || calls != 1 {
		t.Fatal("GET 请求触发了重启")
	}
	s.SetVersion("2.0.0")
	r = call(http.MethodPost, "/api/v1/update/restart", `{"version":"2.0.0"}`)
	if r.Code != http.StatusConflict || calls != 1 {
		t.Fatal("没有待生效版本仍重复安排重启")
	}
}
