//go:build darwin || linux

// 中文说明：在隔离 HOME/XDG 中验证迁移真实格式的登录项，并确保关闭状态不会被升级重新开启。
package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFixedLoginMigrationPreservesUserChoice(t *testing.T) {
	data, bundle := loginFixture(t)
	old := filepath.Join(data, "updates", "versions", "0.1.0-preview.19")
	if err := SetAutostart(filepath.Join(old, "bin", ExecutableName("codex-chronicle-desktop")), data, old, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "autostart", "codex-chronicle.desktop")
	if runtime.GOOS == "darwin" {
		path = filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "local.codex-chronicle.desktop.plist")
	}
	if err := PrepareLoginLauncher(t.Context(), bundle("0.1.0-preview.20", "启动器 20")); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "--launch-active") || strings.Contains(string(content), "updates/versions") {
		t.Fatalf("没有迁移到固定登录入口：%s %v", content, err)
	}
	entry := loginLauncherPath(data)
	if enabled, err := AutostartEnabled(entry, data, ""); err != nil || !enabled {
		t.Fatalf("迁移丢失了开启状态：%v %v", enabled, err)
	}
	if err := SetAutostart(entry, data, "", false); err != nil {
		t.Fatal(err)
	}
	disabled, _ := os.ReadFile(path)
	if err := PrepareLoginLauncher(t.Context(), bundle("0.1.0-preview.21", "启动器 21")); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(disabled) {
		t.Fatal("升级擅自重写了用户关闭的登录项")
	}
}
