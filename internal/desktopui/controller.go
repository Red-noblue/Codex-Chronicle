// 中文说明：菜单栏与托盘共用启动设置、地址与安全退出逻辑；设置窗口不依赖网页服务可用。
package desktopui

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"codex-chronicle.local/internal/desktop"
)

type trayController struct {
	url, executable, dataDir, packageRoot string
}

var controller trayController

func configure(url, executable, dataDir, packageRoot string) {
	controller = trayController{url: url, executable: executable, dataDir: dataDir, packageRoot: packageRoot}
}

func (c trayController) settings() (desktop.StartupSettings, bool, error) {
	settings, err := desktop.LoadStartupSettings(c.dataDir)
	if err != nil {
		return settings, false, err
	}
	executable, root := desktop.LoginEntry(c.executable, c.dataDir, c.packageRoot)
	login, err := desktop.AutostartEnabled(executable, c.dataDir, root)
	return settings, login, err
}

func (c trayController) savePort(port string, fallback bool) error {
	number, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("端口必须是 1 至 65535 之间的整数")
	}
	return desktop.SaveStartupSettings(c.dataDir, desktop.StartupSettings{Port: number, AutoFallback: fallback})
}

func (c trayController) toggleFallback() error {
	settings, err := desktop.LoadStartupSettings(c.dataDir)
	if err != nil {
		return err
	}
	settings.AutoFallback = !settings.AutoFallback
	return desktop.SaveStartupSettings(c.dataDir, settings)
}

func (c trayController) toggleAutostart() error {
	executable, root := desktop.LoginEntry(c.executable, c.dataDir, c.packageRoot)
	enabled, err := desktop.AutostartEnabled(executable, c.dataDir, root)
	if err != nil {
		return err
	}
	if !enabled && executable != c.executable {
		if err := desktop.PrepareLoginLauncher(context.Background(), desktop.LaunchOptions{DataDir: c.dataDir, PackageRoot: c.packageRoot}); err != nil {
			return err
		}
	}
	return desktop.SetAutostart(executable, c.dataDir, root, !enabled)
}

func (c trayController) quit() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return desktop.Quit(ctx, c.dataDir)
}

func (c trayController) open() error { return desktop.OpenBrowser(c.url) }
func (c trayController) logs() error {
	return desktop.OpenBrowser(filepath.Join(c.dataDir, "logs"))
}
