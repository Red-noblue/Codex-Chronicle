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

func authorizePendingRestart(ctx context.Context, options LaunchOptions, targetVersion string) error {
	manager, err := updateManager(options)
	if err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if status.State.ActiveVersion != targetVersion {
		return fmt.Errorf("目标更新版本已变化")
	}
	if status.State.ConfirmedAt != nil {
		return nil
	}
	return manager.AuthorizeOnlineSource(ctx, status.State.Online)
}

// ScheduleUpdateRestart 在返回 HTTP 响应前只启动协作进程；退出与重开不依赖原服务继续存活。
func ScheduleUpdateRestart(ctx context.Context, options LaunchOptions, instanceID, targetVersion string) error {
	if _, err := restartTarget(ctx, options, targetVersion); err != nil {
		return err
	}
	if err := authorizePendingRestart(ctx, options, targetVersion); err != nil {
		return err
	}
	args := []string{"--data-dir", options.DataDir, "--package-root", options.PackageRoot, "--restart-instance", instanceID, "--restart-version", targetVersion}
	if options.NoBrowser {
		args = append(args, "--no-browser")
	} else if options.Background {
		args = append(args, "--background")
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
	installationGuard, err := guardInstallation(ctx, options.DataDir)
	if err != nil {
		return err
	}
	defer installationGuard.Close()
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
	if err := authorizePendingRestart(ctx, options, targetVersion); err != nil {
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
	// 用真实用户配置启动暂停候选；健康确认前不开放操作，失败恢复快照和旧版本。
	if err := trialUpdate(ctx, options, root, instance.Version, targetVersion); err != nil {
		return err
	}
	// 无界面候选已经运行并确认健康，无需再次启动入口；延迟入口可能在用户退出后重开后台。
	if options.NoBrowser {
		return nil
	}
	options.PackageRoot = root
	args := []string{"--data-dir", options.DataDir, "--package-root", root}
	if options.NoBrowser {
		args = append(args, "--no-browser")
	} else if options.Background {
		args = append(args, "--background")
	}
	return startUpdateLauncher(options, args)
}

func confirmUpdateLaunch(ctx context.Context, options LaunchOptions, instance Instance) {
	if err := ConfirmUpdateLaunch(ctx, options, instance); err != nil {
		log.Printf("更新确认或清理暂未完成，将稍后重试：%v", err)
	}
}

// ConfirmUpdateLaunch 由已核验实际健康与开放状态的启动入口确认安装，持久化一次性的恢复窗口。
func ConfirmUpdateLaunch(ctx context.Context, options LaunchOptions, instance Instance) error {
	if options.StartupCheck || instance.StartupPaused {
		return nil
	}
	if _, err := os.Stat(filepath.Join(options.PackageRoot, "update", "trusted-public-key")); os.IsNotExist(err) {
		return nil
	}
	manager, err := updateManager(options)
	if err == nil {
		if err := PrepareLoginLauncher(ctx, options); err != nil {
			return err
		}
		_, err = manager.ConfirmRunning(ctx, instance.Version)
	}
	return err
}
