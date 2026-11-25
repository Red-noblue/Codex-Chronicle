// 中文说明：核验带空格、中文路径的 Windows 登录命令能无损解析，不触碰实际注册表。
package desktop

import (
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
