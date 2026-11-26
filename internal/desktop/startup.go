// 中文说明：保存桌面启动端口与回退策略，并为菜单栏、托盘取得独立的单实例锁。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type StartupSettings struct {
	Port         int  `json:"port"`
	AutoFallback bool `json:"auto_fallback"`
}

func LoadStartupSettings(dataDir string) (StartupSettings, error) {
	settings := StartupSettings{Port: 8787, AutoFallback: true}
	path := filepath.Join(dataDir, "startup.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 {
		return settings, fmt.Errorf("启动设置不是有效的普通文件")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return settings, err
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		return settings, fmt.Errorf("启动设置无法解析，请从端口设置重新保存")
	}
	return settings, settings.Validate()
}

func (s StartupSettings) Validate() error {
	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("端口必须是 1 至 65535 之间的整数")
	}
	return nil
}

func SaveStartupSettings(dataDir string, settings StartupSettings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	dir, err := PrepareDirectory(dataDir)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "startup.json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("启动设置必须为普通文件")
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".startup-*.tmp")
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
	return replaceStartupFile(file.Name(), path)
}

// StartupAddress 保留首次自动模式的地址记忆；主动保存端口后，每次启动优先尝试用户选择。
func StartupAddress(dataDir string) (string, bool, error) {
	settings, err := LoadStartupSettings(dataDir)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(filepath.Join(dataDir, "startup.json")); os.IsNotExist(err) {
		return preferredAddress(dataDir), settings.AutoFallback, nil
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(settings.Port)), settings.AutoFallback, nil
}

// AcquireTray 只锁图标进程，重复点击应用不会出现第二个图标，也不影响后台自己的实例锁。
func AcquireTray(dataDir string) (*os.File, error) {
	dir, err := PrepareDirectory(filepath.Join(dataDir, "runtime"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tray.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("图标锁必须为普通文件")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockInstance(file); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// WaitTrayExit 供外部退出命令与安装器使用；等图标释放程序文件，避免 Windows 升级时启动器仍被占用。
// 图标进程自己的退出动作不能调用此函数，否则会等待自身持有的锁。
func WaitTrayExit(ctx context.Context, dataDir string) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(filepath.Join(dataDir, "runtime", "tray.lock"), os.O_RDWR, 0)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		err = lockInstance(file)
		file.Close()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrAlreadyRunning) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("后台已退出，图标尚未关闭，未强制终止：%w", ctx.Err())
		case <-ticker.C:
		}
	}
}
