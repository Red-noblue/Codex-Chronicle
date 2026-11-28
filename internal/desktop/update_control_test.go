// 中文说明：验证更新控制沿用实例私有凭证、拒绝网页直接访问，并在安装暂停或退出时关闭控制入口。
package desktop

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrivateUpdateControlRequiresInstanceCredential(t *testing.T) {
	lease, err := Acquire(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	called := 0
	control := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/update/control" {
			t.Fatal(r.URL.Path)
		}
		w.WriteHeader(202)
	})
	server := httptest.NewServer(lease.Handler(http.NotFoundHandler(), func() {}, func() bool { return false }, control))
	defer server.Close()
	if err = lease.Publish(strings.TrimPrefix(server.URL, "http://"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	r, err := http.Post(server.URL+controlPath+"/update/control", "application/json", strings.NewReader(`{"action":"pause"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 || called != 0 {
		t.Fatal("无凭证请求进入私有更新入口")
	}
	instance := lease.Instance
	if err = instance.ControlUpdateDownload(t.Context(), "pause"); err != nil || called != 1 {
		t.Fatal(err, called)
	}
	lease.SetStartupPaused(true)
	if err = instance.ControlUpdateDownload(t.Context(), "resume"); err == nil || called != 1 {
		t.Fatal("安装暂停时仍接受了下载操作")
	}
}
