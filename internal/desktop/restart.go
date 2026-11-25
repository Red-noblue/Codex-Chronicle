// 中文说明：通过独立启动器协作完成更新重启，核对实例身份、等待数据任务与旧托盘退出，新版本健康后再清理缓存。
package desktop

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	updatepkg "codex-chronicle.local/internal/update"
)

func updateManager(options LaunchOptions) (*updatepkg.Manager, error) {
	keys, err := updatepkg.LoadTrustedPublicKeys(filepath.Join(options.PackageRoot, "update", "trusted-public-key"))
	if err != nil {
		return nil, err
	}
	return updatepkg.NewManagerWithOptions(filepath.Join(options.DataDir, "updates"), updatepkg.ManagerOptions{
		RequireSignature: true, TrustedPublicKeys: keys, Online: updatepkg.OnlineConfig{FeedURL: options.UpdateFeedURL},
	})
}

func restartTarget(ctx context.Context, options LaunchOptions, targetVersion string) (string, error) {
	manager, err := updateManager(options)
	if err != nil {
		return "", err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return "", err
	}
	if targetVersion == "" || status.State.ActiveVersion != targetVersion || status.RecoveryRequired {
		return "", fmt.Errorf("目标更新版本已变化或需要恢复，未重启")
	}
	root, err := ActivePackageRoot(options.PackageRoot, options.DataDir)
	if err != nil {
		return "", err
	}
	if err := manager.VerifyVersionRoot(ctx, root); err != nil {
		return "", err
	}
	return root, nil
}

// ScheduleUpdateRestart 在返回 HTTP 响应前只启动协作进程；退出与重开不依赖原服务继续存活。
func ScheduleUpdateRestart(ctx context.Context, options LaunchOptions, instanceID, targetVersion string) error {
	if _, err := restartTarget(ctx, options, targetVersion); err != nil {
		return err
	}
	args := []string{"--data-dir", options.DataDir, "--package-root", options.PackageRoot, "--restart-instance", instanceID, "--restart-version", targetVersion}
	if options.NoBrowser {
		args = append(args, "--no-browser")
	}
	return startUpdateLauncher(options, args)
}

func startUpdateLauncher(options LaunchOptions, args []string) error {
	dir, err := PrepareDirectory(filepath.Join(options.DataDir, "logs"))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "update-restart.log")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("更新日志必须是普通文件")
	}
	output, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer output.Close()
	command := exec.Command(filepath.Join(options.PackageRoot, "bin", ExecutableName("codex-chronicle-desktop")), args...)
	command.Dir, command.Stdout, command.Stderr = options.DataDir, output, output
	detach(command)
	if err := command.Start(); err != nil {
		return fmt.Errorf("无法启动更新重启协作进程：%w", err)
	}
	go func() { _ = command.Wait() }()
	return nil
}

// RestartAfterUpdate 只允许退出请求中绑定的旧实例；等待期间不强杀任何进程。
func RestartAfterUpdate(ctx context.Context, options LaunchOptions, instanceID, targetVersion string) error {
	dir, err := PrepareDirectory(filepath.Join(options.DataDir, "runtime"))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "restart.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("重启锁必须是普通文件")
	}
	guard, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer guard.Close()
	if err := lockInstance(guard); err != nil {
		return fmt.Errorf("已有更新重启正在进行：%w", err)
	}
	instance, err := Running(ctx, options.DataDir)
	if err != nil {
		return err
	}
	if instance.ID != instanceID || instance.Stopping {
		return fmt.Errorf("后台实例身份已变化，未重复退出或重启")
	}
	root, err := restartTarget(ctx, options, targetVersion)
	if err != nil {
		return err
	}
	if err := Quit(ctx, options.DataDir); err != nil {
		return err
	}
	trayCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := WaitTrayExit(trayCtx, options.DataDir); err != nil {
		return err
	}
	// 等待退出期间若用户另行切换了版本，不能悄悄执行过期更新。
	if _, err := restartTarget(ctx, options, targetVersion); err != nil {
		return err
	}
	options.PackageRoot = root
	args := []string{"--data-dir", options.DataDir, "--package-root", root}
	if options.NoBrowser {
		args = append(args, "--no-browser")
	}
	return startUpdateLauncher(options, args)
}

func confirmUpdateLaunch(ctx context.Context, options LaunchOptions, instance Instance) {
	if options.StartupCheck || instance.StartupPaused {
		return
	}
	if _, err := os.Stat(filepath.Join(options.PackageRoot, "update", "trusted-public-key")); os.IsNotExist(err) {
		return
	}
	manager, err := updateManager(options)
	if err == nil {
		_, err = manager.ConfirmRunning(ctx, instance.Version)
	}
	if err != nil {
		log.Printf("更新文件暂未自动清理，将在下次启动时重试：%v", err)
	}
}
