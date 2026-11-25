// 中文说明：从安装目录定位程序和静态页面，启动独立后台并在身份与版本核验后打开浏览器。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Config 仅描述用户显式选择的工作区和真实执行依赖，不包含账户令牌。
type Config struct {
	ScanRoot              string `json:"scan_root,omitempty"`
	ArchiveRoot           string `json:"archive_root,omitempty"`
	RealConversationWrite bool   `json:"real_conversation_write,omitempty"`
	Python                string `json:"python,omitempty"`
	DaemonSocket          string `json:"daemon_socket,omitempty"`
}

type LaunchOptions struct {
	PackageRoot     string
	DataDir         string
	Address         string
	AutoPort        bool
	ScanRoot        string
	StartupCheck    bool
	ExpectedVersion string
	UpdateFeedURL   string
}

// ActivePackageRoot 根据独立更新目录中的版本指针选择已激活载荷；没有激活版本时保留安装包本体。
func ActivePackageRoot(packageRoot, dataDir string) (string, error) {
	packageRoot, err := filepath.Abs(filepath.Clean(packageRoot))
	if err != nil {
		return "", err
	}
	dataDir, err = filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return "", err
	}
	statePath := filepath.Join(dataDir, "updates", "install-state.json")
	data, err := os.ReadFile(statePath)
	if os.IsNotExist(err) {
		return packageRoot, nil
	}
	if err != nil {
		return "", err
	}
	var state struct {
		ActiveVersion string `json:"active_version"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", fmt.Errorf("读取已激活更新版本失败：%w", err)
	}
	version := filepath.Clean(strings.TrimSpace(state.ActiveVersion))
	if version == "." || version == "" {
		return packageRoot, nil
	}
	if version != strings.TrimSpace(state.ActiveVersion) || strings.ContainsAny(version, `/\\`) || version == ".." {
		return "", fmt.Errorf("已激活更新版本无效：%q", state.ActiveVersion)
	}
	active := filepath.Join(dataDir, "updates", "versions", version)
	info, err := os.Lstat(active)
	if err != nil {
		return "", fmt.Errorf("已激活版本目录不可用：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("已激活版本路径不是目录")
	}
	return active, nil
}

func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Codex Chronicle"), nil
	case "windows":
		if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
			return filepath.Join(dir, "Codex Chronicle"), nil
		}
		return filepath.Join(home, "AppData", "Local", "Codex Chronicle"), nil
	default:
		if dir := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(dir) {
			return filepath.Join(dir, "codex-chronicle"), nil
		}
		return filepath.Join(home, ".local", "share", "codex-chronicle"), nil
	}
}

func ExecutableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// PackageRoot 同时支持 .app/Contents/Resources/app 和便携包的 bin/ 布局。
func PackageRoot(executable string) string {
	dir := filepath.Dir(executable)
	if filepath.Base(dir) == "MacOS" && filepath.Base(filepath.Dir(dir)) == "Contents" {
		return filepath.Join(filepath.Dir(dir), "Resources", "app")
	}
	return filepath.Dir(dir)
}

func LoadConfig(dataDir string) (Config, error) {
	var config Config
	path := filepath.Join(dataDir, "launcher.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return config, fmt.Errorf("启动配置不是有效的普通文件")
	}
	file, err := os.Open(path)
	if err != nil {
		return config, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return config, fmt.Errorf("读取 launcher.json 失败：%w", err)
	}
	return config, nil
}

// DaemonArguments 只传固定命名参数；用户数据和恢复材料始终位于安装包之外。
func DaemonArguments(options LaunchOptions, config Config) ([]string, error) {
	host, _, err := net.SplitHostPort(options.Address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, fmt.Errorf("启动地址必须是本机回环 IP 和端口")
	}
	if options.ScanRoot != "" {
		config.ScanRoot = options.ScanRoot
	}
	args := []string{
		"--address", options.Address,
		"--desktop-state-dir", filepath.Join(options.DataDir, "runtime"),
		"--web-root", filepath.Join(options.PackageRoot, "web"),
		"--archive-db", filepath.Join(options.DataDir, "archive.db"),
		"--workspace-config", filepath.Join(options.DataDir, "workspaces.json"),
		"--auth-state", filepath.Join(options.DataDir, "auth.json"),
		"--trash-settings", filepath.Join(options.DataDir, "trash-settings.json"),
		"--github-settings", filepath.Join(options.DataDir, "github-settings.json"),
		"--github-archive-selection", filepath.Join(options.DataDir, "github-archive-selection.json"),
		"--remote-recovery-root", filepath.Join(options.DataDir, "remote-recovery"),
		"--journal-root", filepath.Join(options.DataDir, "lifecycle"),
	}
	trustedKey := filepath.Join(options.PackageRoot, "update", "trusted-public-key")
	if info, keyErr := os.Stat(trustedKey); keyErr == nil && info.Mode().IsRegular() {
		args = append(args,
			"--install-root", filepath.Join(options.DataDir, "updates"),
			"--update-require-signature", "--update-trusted-public-key-file", trustedKey)
		if strings.TrimSpace(options.UpdateFeedURL) != "" {
			args = append(args, "--update-feed-url", options.UpdateFeedURL)
		}
	}
	if options.AutoPort {
		args = append(args, "--desktop-auto-port")
	}
	if options.StartupCheck {
		args = append(args, "--desktop-start-paused")
	}
	if config.ScanRoot != "" {
		args = append(args, "--scan-root", config.ScanRoot)
	}
	if config.ArchiveRoot != "" {
		args = append(args, "--archive-root", config.ArchiveRoot)
	}
	if config.RealConversationWrite {
		if runtime.GOOS != "darwin" || config.ScanRoot == "" || !filepath.IsAbs(config.Python) || !filepath.IsAbs(config.DaemonSocket) {
			return nil, fmt.Errorf("真实执行当前仅支持 macOS，必须显式配置 scan_root、python 和 daemon_socket 的绝对路径")
		}
		args = append(args, "--real-conversation-write", "--authorized-python", config.Python,
			"--authorized-daemon-socket", config.DaemonSocket,
			"--authorized-trash-bridge", filepath.Join(options.PackageRoot, "bin", ExecutableName("chronicle-native-trash")))
	}
	return args, nil
}

func Running(ctx context.Context, dataDir string) (Instance, error) {
	instance, err := LoadInstance(filepath.Join(dataDir, "runtime"))
	if err != nil {
		return Instance{}, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	status, err := instance.Status(probeCtx)
	if err != nil {
		return Instance{}, err
	}
	instance.Stopping, instance.StartupPaused = status.Stopping, status.StartupPaused
	return instance, nil
}

func Launch(ctx context.Context, options LaunchOptions) (Instance, error) {
	var err error
	options.DataDir, err = PrepareDirectory(options.DataDir)
	if err != nil {
		return Instance{}, err
	}
	if !options.StartupCheck {
		guard, err := guardInstallation(ctx, options.DataDir)
		if err != nil {
			return Instance{}, err
		}
		defer guard.Close()
	}
	if instance, err := Running(ctx, options.DataDir); err == nil {
		if instance.Stopping {
			return Instance{}, fmt.Errorf("已有后台正在退出，请等待任务完成")
		}
		if options.StartupCheck && (instance.Version != options.ExpectedVersion || !instance.StartupPaused) {
			return Instance{}, fmt.Errorf("已有后台并非本次暂停检查的候选版本")
		}
		if instance.StartupPaused && !options.StartupCheck {
			return Instance{}, fmt.Errorf("后台仍在安装检查，尚未开放操作")
		}
		if !options.AutoPort && options.Address != "" && options.Address != "127.0.0.1:0" && instance.URL != "http://"+options.Address {
			return Instance{}, fmt.Errorf("已有后台运行于 %s，与显式指定的地址 %s 不同；请先安全退出已有后台", instance.URL, options.Address)
		}
		if err := instance.CheckHealth(ctx); err != nil {
			return Instance{}, err
		}
		return instance, nil
	}
	options, err = resolveUpdatePackage(ctx, options)
	if err != nil {
		return Instance{}, err
	}
	if options.AutoPort {
		options.Address, options.AutoPort, err = StartupAddress(options.DataDir)
		if err != nil {
			return Instance{}, err
		}
	}
	config, err := LoadConfig(options.DataDir)
	if err != nil {
		return Instance{}, err
	}
	args, err := DaemonArguments(options, config)
	if err != nil {
		return Instance{}, err
	}
	for _, name := range []string{"runtime", "logs"} {
		if _, err := PrepareDirectory(filepath.Join(options.DataDir, name)); err != nil {
			return Instance{}, err
		}
	}
	for _, path := range []string{filepath.Join(options.PackageRoot, "bin", ExecutableName("codex-chronicle")), filepath.Join(options.PackageRoot, "web", "index.html")} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return Instance{}, fmt.Errorf("安装包缺少普通文件：%s", path)
		}
	}
	logPath := filepath.Join(options.DataDir, "logs", "daemon.log")
	if info, err := os.Lstat(logPath); err == nil && !info.Mode().IsRegular() {
		return Instance{}, fmt.Errorf("日志路径不是普通文件")
	}
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Instance{}, err
	}
	defer output.Close()
	command := exec.Command(filepath.Join(options.PackageRoot, "bin", ExecutableName("codex-chronicle")), args...)
	command.Dir, command.Stdout, command.Stderr = options.DataDir, output, output
	detach(command)
	if err := command.Start(); err != nil {
		return Instance{}, fmt.Errorf("无法启动后台：%w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var exitedAt time.Time
	for {
		instance, err := Running(ctx, options.DataDir)
		if err == nil {
			if instance.PID == command.Process.Pid && instance.Version == "" {
				return Instance{}, fmt.Errorf("后台缺少版本标识")
			}
			if options.ExpectedVersion != "" && instance.Version != options.ExpectedVersion {
				return Instance{}, fmt.Errorf("后台版本 %s 与所选载荷 %s 不符", instance.Version, options.ExpectedVersion)
			}
			if options.StartupCheck && !instance.StartupPaused {
				return Instance{}, fmt.Errorf("候选版本身份或暂停状态不符")
			}
			if err := instance.CheckHealth(ctx); err != nil {
				return Instance{}, err
			}
			return instance, nil
		}
		if !exitedAt.IsZero() && time.Since(exitedAt) > 2*time.Second {
			return Instance{}, fmt.Errorf("后台启动失败，请查看：%s", logPath)
		}
		select {
		case <-ctx.Done():
			return Instance{}, fmt.Errorf("等待后台启动超时；日志：%s：%w", logPath, ctx.Err())
		case exitErr := <-done:
			if exitErr != nil {
				return Instance{}, fmt.Errorf("后台未能在 %s 启动：%w；请查看：%s", options.Address, exitErr, logPath)
			}
			exitedAt = time.Now()
		case <-ticker.C:
		}
	}
}

// Quit 通过已认证控制通道请求退出，并等待内核锁释放；超时只报告，不强杀后台。
func Quit(ctx context.Context, dataDir string) error {
	instance, err := Running(ctx, dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		// 崩溃后的旧记录不能指向当前进程；只有内核锁确已释放时才视为已退出。
		file, lockErr := os.OpenFile(filepath.Join(dataDir, "runtime", "instance.lock"), os.O_RDWR, 0)
		if os.IsNotExist(lockErr) {
			return nil
		}
		if lockErr == nil {
			lockErr = lockInstance(file)
			_ = file.Close()
			if lockErr == nil {
				return nil
			}
		}
		return fmt.Errorf("无法核对后台身份，未终止任何进程：%w", err)
	}
	if err := instance.RequestQuit(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(filepath.Join(dataDir, "runtime", "instance.lock"), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = lockInstance(file)
		_ = file.Close()
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrAlreadyRunning) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("后台仍在等待任务完成，未强制退出：%w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func OpenBrowser(address string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("/usr/bin/open", address)
	case "windows":
		command = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address)
	default:
		command = exec.Command("xdg-open", address)
	}
	return command.Run()
}
