// 中文说明：验证退出后仍能复用上次成功地址，损坏或越界的偏好不会改变本机监听范围。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRememberAddressSurvivesExit(t *testing.T) {
	data := t.TempDir()
	if got := preferredAddress(data); got != defaultAddress {
		t.Fatalf("首次启动地址异常：%s", got)
	}
	lease, err := Acquire(filepath.Join(data, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.file.Close() })
	if err := lease.Publish("127.0.0.1:49152", "test"); err != nil {
		t.Fatal(err)
	}
	if err := lease.RememberAddress(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if got := preferredAddress(data); got != "127.0.0.1:49152" {
		t.Fatalf("退出后丢失地址：%s", got)
	}
	if _, err := LoadInstance(filepath.Join(data, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("地址偏好被误当作活动实例：%v", err)
	}
}

func TestInvalidSavedAddressUsesDefault(t *testing.T) {
	data := t.TempDir()
	if err := os.Mkdir(filepath.Join(data, "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"0.0.0.0:8787", "example.com:8787", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:-1", "127.0.0.1:no-port"} {
		body, _ := json.Marshal(map[string]string{"address": address})
		if err := os.WriteFile(filepath.Join(data, "runtime", "last-address.json"), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if actual := preferredAddress(data); actual != defaultAddress {
			t.Fatalf("接受了非法保存地址 %s：%s", address, actual)
		}
	}
}

func TestAutoPortIsOnlyPassedWhenRequested(t *testing.T) {
	for _, auto := range []bool{false, true} {
		args, err := DaemonArguments(LaunchOptions{Address: defaultAddress, AutoPort: auto}, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.Join(args, " "), "--desktop-auto-port") != auto {
			t.Fatalf("自动端口参数不符合启动模式：%v", args)
		}
	}
}
