// 中文说明：在隔离 XDG 目录中验证登录启动项启停、Exec 特殊字符转义及非法路径拒绝。
package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxLoginEntry(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	executable, data, payload := `/opt/工具 & %x/bin/launcher`, `/home/测试/$HOME`, "/opt/package with space"
	if enabled, err := AutostartEnabled(executable, data, payload); err != nil || enabled {
		t.Fatalf("默认自启动错误：%v %v", enabled, err)
	}
	for _, enabled := range []bool{true, false, true} {
		if err := SetAutostart(executable, data, payload, enabled); err != nil {
			t.Fatal(err)
		}
		got, err := AutostartEnabled(executable, data, payload)
		if err != nil || got != enabled {
			t.Fatalf("登录设置未保存：%v %v", got, err)
		}
	}
	path := filepath.Join(config, "autostart", "codex-chronicle.desktop")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, wanted := range []string{`"/opt/工具 & %%x/bin/launcher"`, `"/home/测试/\\$HOME"`, `"--background"`, "Hidden=false"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("启动项未正确转义 %q：%s", wanted, text)
		}
	}
	if err := SetAutostart("/opt/broken\nInjected=true", data, payload, true); err == nil {
		t.Fatal("接受了换行注入")
	}
	after, _ := os.ReadFile(path)
	if string(after) != text {
		t.Fatal("非法路径破坏了原设置")
	}
}
func TestLinuxLoginUsesXDGAndHomeFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "relative-is-not-valid")
	path, err := linuxLoginPath()
	if err != nil || path != filepath.Join(home, ".config", "autostart", "codex-chronicle.desktop") {
		t.Fatalf("默认 XDG 路径错误：%s %v", path, err)
	}
}
