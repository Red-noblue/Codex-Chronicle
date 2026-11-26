// 中文说明：验证更新协作进程不能退出身份已变化的实例，也不能在更新目标未通过核验时退出后台。
package desktop

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestartRejectsStaleInstanceBeforeStopping(t *testing.T) {
	data := t.TempDir()
	lease, err := Acquire(filepath.Join(data, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	stopped := false
	server := httptest.NewServer(lease.Handler(http.NotFoundHandler(), func() { stopped = true }, func() bool { return false }))
	defer server.Close()
	if err := lease.Publish(strings.TrimPrefix(server.URL, "http://"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	options := LaunchOptions{DataDir: data, PackageRoot: t.TempDir()}
	if err := RestartAfterUpdate(t.Context(), options, "过期实例", "2.0.0"); err == nil {
		t.Fatal("接受了过期实例")
	}
	if stopped {
		t.Fatal("退出了并非本次更新绑定的实例")
	}
	if err := RestartAfterUpdate(t.Context(), options, lease.Instance.ID, "2.0.0"); err == nil {
		t.Fatal("未验证签名与目标版本就重启")
	}
	if stopped {
		t.Fatal("目标核验失败却退出了旧后台")
	}
}
