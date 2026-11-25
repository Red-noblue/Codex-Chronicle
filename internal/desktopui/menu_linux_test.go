// 中文说明：使用隔离的原生对话框进程替身检查设置保存、取消与后台退出取消弹窗，不连接用户桌面。
package desktopui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-chronicle.local/internal/desktop"
)

func TestLinuxSettingsDialogResult(t *testing.T) {
	for _, test := range []struct {
		name, script string
		initial      desktop.StartupSettings
		want         desktop.StartupSettings
		exists       bool
	}{
		{"选择端口并关闭回退", "printf '9321\\037关闭\\n'", desktop.StartupSettings{Port: 8787, AutoFallback: true}, desktop.StartupSettings{Port: 9321}, true},
		{"留空保留现有值", "printf '\\037\\n'", desktop.StartupSettings{Port: 9000}, desktop.StartupSettings{Port: 9000}, true},
		{"取消不生成设置", "exit 1", desktop.StartupSettings{}, desktop.StartupSettings{Port: 8787, AutoFallback: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := t.TempDir()
			t.Setenv("DISPLAY", ":chronicle-test")
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("PATH", bin)
			if err := os.WriteFile(filepath.Join(bin, "zenity"), []byte("#!/bin/sh\n"+test.script+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			if test.initial.Port != 0 {
				if err := desktop.SaveStartupSettings(dir, test.initial); err != nil {
					t.Fatal(err)
				}
			}
			configure("http://127.0.0.1:8787", "test-launcher", dir, dir)
			linuxSettings(context.Background())
			settings, err := desktop.LoadStartupSettings(dir)
			if err != nil || settings != test.want {
				t.Fatalf("对话框保存行为错误：%+v %v", settings, err)
			}
			_, err = os.Stat(filepath.Join(dir, "startup.json"))
			if test.exists != (err == nil) {
				t.Fatalf("取消/保存状态错误：%v", err)
			}
		})
	}
}
func TestLinuxDialogClosesWhenBackendStops(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("DISPLAY", ":chronicle-test")
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "zenity"), []byte("#!/bin/sh\nexec /bin/sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := linuxDialog(ctx, "--entry")
	if err == nil || ctx.Err() == nil || time.Since(start) > time.Second {
		t.Fatalf("后台退出后弹窗仍未关闭：%v", err)
	}
}
