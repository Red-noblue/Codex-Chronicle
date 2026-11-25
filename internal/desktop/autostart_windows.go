// 中文说明：仅使用 Windows 当前用户的 Run 注册表项控制登录自启动，不申请管理员权限。
package desktop

import (
	"errors"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const loginRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const loginRegistryName = "CodexChronicle"

func loginCommand(executable, dataDir, packageRoot string) string {
	return windows.ComposeCommandLine([]string{executable, "--background", "--data-dir", dataDir, "--package-root", packageRoot})
}

func AutostartEnabled(executable, dataDir, packageRoot string) (bool, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, loginRegistryPath, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer key.Close()
	value, _, err := key.GetStringValue(loginRegistryName)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	return value == loginCommand(executable, dataDir, packageRoot), err
}

func SetAutostart(executable, dataDir, packageRoot string, enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, loginRegistryPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if enabled {
		return key.SetStringValue(loginRegistryName, loginCommand(executable, dataDir, packageRoot))
	}
	err = key.DeleteValue(loginRegistryName)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}
