// 中文说明：保证已保存的联网代理不会阻断离线验签与启动确认，同时保留在线管理器的代理设置和显式配置校验。
package update

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineManagerIgnoresPersistedOnlineProxy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "updates")
	options := ManagerOptions{Online: OnlineConfig{FeedURL: "https://updates.example.invalid/feed.json"}}
	online, err := NewManagerWithOptions(root, options)
	if err != nil {
		t.Fatal(err)
	}
	const proxy = "http://127.0.0.1:7890"
	if err := online.SetOnlineProxy(proxy); err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(root, "online-proxy.json")
	before, err := os.ReadFile(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	offline, err := NewManager(root)
	if err != nil {
		t.Fatalf("保存代理后无法建立离线管理器：%v", err)
	}
	status, err := offline.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.OnlineConfigured || status.OnlineProxyURL != "" {
		t.Fatalf("离线管理器不应启用联网代理：%+v", status)
	}
	after, err := os.ReadFile(proxyPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("离线初始化改写了保存的代理：%v", err)
	}
	reopened, err := NewManagerWithOptions(root, options)
	if err != nil {
		t.Fatal(err)
	}
	status, err = reopened.Status(t.Context())
	if err != nil || !status.OnlineConfigured || status.OnlineProxyURL != proxy {
		t.Fatalf("在线管理器未保留代理：%+v %v", status, err)
	}
	if _, err := NewManagerWithOptions(filepath.Join(t.TempDir(), "invalid"), ManagerOptions{Online: OnlineConfig{ProxyURL: proxy}}); err == nil {
		t.Fatal("显式指定代理而未配置更新源仍应拒绝")
	}
}
