// 中文说明：通过 XDG autostart 的当前用户桌面启动项管理登录自启动，兼容 Wayland、X11，不创建系统服务。
package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func linuxLoginPath() (string, error) {
	config := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(config) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		config = filepath.Join(home, ".config")
	}
	return filepath.Join(config, "autostart", "codex-chronicle.desktop"), nil
}

// desktopExecArgument 分别处理 desktop entry 字符串转义、Exec 引号及字段码；不经过 shell。
func desktopExecArgument(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\n\r") {
		return "", fmt.Errorf("登录启动路径不能包含换行或空字符")
	}
	value = strings.ReplaceAll(value, "%", "%%")
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`).Replace(value)
	value = `"` + value + `"`
	return strings.ReplaceAll(value, `\`, `\\`), nil
}

func linuxLoginEntry(executable, dataDir, packageRoot string, enabled bool) ([]byte, error) {
	arguments := loginArguments(executable, dataDir, packageRoot)
	for i, argument := range arguments {
		quoted, err := desktopExecArgument(argument)
		if err != nil {
			return nil, err
		}
		arguments[i] = quoted
	}
	hidden := "true"
	if enabled {
		hidden = "false"
	}
	return []byte("# 中文说明：Codex Chronicle 当前用户登录启动项，由应用设置管理。\n[Desktop Entry]\nType=Application\nName=Codex Chronicle\nComment=登录后启动本地工作台后台，不主动打开网页\nExec=" + strings.Join(arguments, " ") + "\nIcon=folder-documents\nTerminal=false\nHidden=" + hidden + "\n"), nil
}

func AutostartEnabled(executable, dataDir, packageRoot string) (bool, error) {
	path, err := linuxLoginPath()
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return false, fmt.Errorf("登录启动项不是有效的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return matchesLoginEntry(string(data), dataDir, func(entry, root string) (string, error) {
		body, err := linuxLoginEntry(entry, dataDir, root, true)
		return string(body), err
	}, executable, packageRoot)
}

func SetAutostart(executable, dataDir, packageRoot string, enabled bool) error {
	data, err := linuxLoginEntry(executable, dataDir, packageRoot, enabled)
	if err != nil {
		return err
	}
	path, err := linuxLoginPath()
	if err != nil {
		return err
	}
	dir, err := PrepareDirectory(filepath.Dir(path))
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("登录启动项必须为普通文件")
	}
	file, err := os.CreateTemp(dir, ".chronicle-login-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), path)
}
