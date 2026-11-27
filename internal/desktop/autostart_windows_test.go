// 中文说明：核验带空格、中文路径的 Windows 登录命令能无损解析，不触碰实际注册表。
package desktop

import (
	"path/filepath"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLoginCommand(t *testing.T) {
	executable, data, payload := `C:\Program Files\Codex Chronicle\bin\launcher.exe`, `C:\Users\测试用户\AppData\Local\Chronicle`, `D:\便携应用\Chronicle`
	arguments, err := windows.DecomposeCommandLine(loginCommand(executable, data, payload))
	want := []string{executable, "--background", "--data-dir", data, "--package-root", payload}
	if err != nil || !reflect.DeepEqual(arguments, want) {
		t.Fatalf("登录启动参数错误：%q %v", arguments, err)
	}
}

func TestWindowsStableLoginCommandAndLegacyMigration(t *testing.T) {
	data := t.TempDir()
	entry := loginLauncherPath(data)
	arguments, err := windows.DecomposeCommandLine(loginCommand(entry, data, ""))
	if err != nil || !reflect.DeepEqual(arguments, []string{entry, "--background", "--data-dir", data, "--launch-active"}) {
		t.Fatalf("固定登录命令仍然依赖版本目录：%v %v", arguments, err)
	}
	old := filepath.Join(data, "updates", "versions", "0.1.0-preview.19")
	saved := loginCommand(filepath.Join(old, "bin", ExecutableName("codex-chronicle-desktop")), data, old)
	matched, err := matchesLoginEntry(saved, data, func(executable, root string) (string, error) {
		return loginCommand(executable, data, root), nil
	}, entry, "")
	if err != nil || !matched {
		t.Fatalf("没有识别旧版已启用的登录命令：%v %v", matched, err)
	}
}
