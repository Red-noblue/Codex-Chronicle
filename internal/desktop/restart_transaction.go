// 中文说明：在线更新在开放请求前保存项目快照并暂停试启动；失败恢复数据库与旧版本，提交后绝不用旧快照覆盖新数据。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	updatepkg "codex-chronicle.local/internal/update"
	workspacepkg "codex-chronicle.local/internal/workspace"
)

const restartJournalName = "restart-journal.json"

type restartFile struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256,omitempty"`
	Database bool   `json:"database,omitempty"`
}

func restartDatabasePath(data, path string) bool {
	for _, name := range installProfileFiles {
		if name != "archive.db" && path == filepath.Join(data, name) {
			return false
		}
	}
	return true // 路径清单的其余条目只能是主数据库或注册的工作区数据库。
}

type restartJournal struct {
	Version  string        `json:"version"`
	Previous string        `json:"previous"`
	Work     string        `json:"work"`
	Phase    string        `json:"phase"`
	Files    []restartFile `json:"files"`
}

func saveRestartJournal(data string, j restartJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeInstallFile(filepath.Join(data, "updates", restartJournalName), b)
}

func restartProfilePaths(data string) ([]string, error) {
	return restartProfilePathsFrom(data, filepath.Join(data, "workspaces.json"))
}

func restartProfilePathsFrom(data, registryPath string) ([]string, error) {
	paths := []string{}
	for _, name := range installProfileFiles {
		if name != "updates/install-state.json" {
			paths = append(paths, filepath.Join(data, name))
		}
	}
	// 初始化会打开注册的工作区数据库，外置项目数据库也必须有对应的恢复快照。
	registry, err := workspacepkg.NewFileStore(registryPath)
	if err != nil {
		return nil, err
	}
	reg, err := registry.Load(context.Background())
	if err != nil {
		return nil, err
	}
	for _, w := range reg.Workspaces {
		if w.ArchiveDB != "" {
			paths = append(paths, w.ArchiveDB)
		}
	}
	seen, result := map[string]bool{}, []string{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("项目快照路径必须是绝对路径")
		}
		for current := path; ; current = filepath.Dir(current) {
			if strings.EqualFold(filepath.Base(current), ".codex") {
				return nil, fmt.Errorf("快照不能进入原始 .codex")
			}
			if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("快照路径不能包含链接")
			}
			if filepath.Dir(current) == current {
				break
			}
		}
		if !seen[path] {
			seen[path] = true
			result = append(result, path)
		}
	}
	return result, nil
}

func snapshotRestartProfile(data string, j *restartJournal) error {
	return snapshotRestartProfileWithSpaceCheck(data, j, updatepkg.RequireDiskSpace)
}

func snapshotRestartProfileWithSpaceCheck(data string, j *restartJournal, check func(string, int64, string) error) error {
	lease, err := Acquire(filepath.Join(data, "runtime"))
	if err != nil {
		return err
	}
	defer lease.Close()
	paths, err := restartProfilePaths(data)
	if err != nil {
		return err
	}
	var bytes int64
	for _, path := range paths {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("更新快照来源不是普通文件")
		}
		if info.Size() > (1<<63-1)-bytes {
			return fmt.Errorf("更新快照体积超过有效范围")
		}
		bytes += info.Size()
	}
	if err := check(j.Work, bytes, "备份项目数据与设置"); err != nil {
		return err
	}
	for _, path := range paths {
		database := restartDatabasePath(data, path)
		// 后台已正常关闭；非空 WAL 表示快照还不能安全代表完整数据库。
		if info, err := os.Stat(path + "-wal"); database && err == nil && info.Size() != 0 {
			return fmt.Errorf("数据库 WAL 尚未合并，停止试启动")
		}
		entry := restartFile{Path: path, Database: database}
		if _, err := os.Lstat(path); err == nil {
			entry.SHA256, err = copyInstallFile(path, filepath.Join(j.Work, fmt.Sprint(len(j.Files))))
			if err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		j.Files = append(j.Files, entry)
	}
	j.Phase = "trial"
	return saveRestartJournal(data, *j)
}

func finishRestartJournal(data string, j restartJournal) error {
	// 先持久化完成标记；清理中断后只重试删除，绝不重复恢复已经开放的数据库。
	j.Phase = "finished"
	if err := saveRestartJournal(data, j); err != nil {
		return err
	}
	if err := os.RemoveAll(j.Work); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(data, "updates", restartJournalName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func trialUpdate(ctx context.Context, options LaunchOptions, root, previous, target string) error {
	manager, err := updateManager(options)
	if err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if err := manager.AuthorizeOnlineSource(ctx, status.State.Online); err != nil {
		if errors.Is(err, updatepkg.ErrOnlineReleaseWithdrawn) {
			if restoreErr := manager.FailStartup(ctx, target, previous, err.Error()); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		} else {
			if restoreErr := manager.PostponeStartup(ctx, target, previous, err.Error()); restoreErr != nil {
				return errors.Join(err, restoreErr)
			}
		}
		return errors.Join(err, launchRecoveredVersion(ctx, options))
	}
	work, err := os.MkdirTemp(manager.Root(), "restart-profile-")
	if err != nil {
		return err
	}
	j := restartJournal{Version: target, Previous: previous, Work: work, Phase: "preparing"}
	if err := saveRestartJournal(options.DataDir, j); err != nil {
		return err
	}
	if err := snapshotRestartProfile(options.DataDir, &j); err != nil {
		// 没有启动候选，项目数据尚未变化；恢复指针后直接重新打开旧版。
		var spaceError *updatepkg.InsufficientSpaceError
		restore := manager.FailStartup
		if errors.As(err, &spaceError) || errors.Is(err, updatepkg.ErrDiskSpaceUnavailable) {
			restore = manager.PostponeStartup
		}
		if rollbackErr := restore(ctx, target, previous, err.Error()); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		if cleanupErr := finishRestartJournal(options.DataDir, j); cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		return errors.Join(err, launchRecoveredVersion(ctx, options))
	}
	launchCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	instance, err := launchPausedVersion(launchCtx, options, root, target)
	cancel()
	if err != nil {
		recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if recoveryErr := recoverRestartJournal(recoveryCtx, options); recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		return errors.Join(fmt.Errorf("新版本启动失败，已恢复旧版本：%w", err), launchRecoveredVersion(recoveryCtx, options))
	}
	// 持久化提交意图必须早于开放 HTTP；此后只完成提交，绝不回写旧快照。
	j.Phase = "committing"
	if err := saveRestartJournal(options.DataDir, j); err != nil {
		return err
	}
	if err := instance.Activate(ctx); err != nil {
		return err
	}
	if _, err := manager.ConfirmRunning(ctx, target); err != nil {
		return err
	}
	return finishRestartJournal(options.DataDir, j)
}

func launchPausedVersion(ctx context.Context, options LaunchOptions, root, target string) (Instance, error) {
	// 使用候选自身入口，继承候选构建内置的更新渠道，不能注入旧启动器的 feed。
	args := []string{"--startup-check", "--no-browser", "--data-dir", options.DataDir, "--package-root", root}
	if !options.AutoPort && options.Address != "" {
		args = append(args, "--address", options.Address)
	}
	if options.ScanRoot != "" {
		args = append(args, "--scan-root", options.ScanRoot)
	}
	command := exec.CommandContext(ctx, filepath.Join(root, "bin", ExecutableName("codex-chronicle-desktop")), args...)
	if err := command.Run(); err != nil {
		return Instance{}, err
	}
	instance, err := Running(ctx, options.DataDir)
	if err != nil {
		return Instance{}, err
	}
	if instance.Version != target || !instance.StartupPaused {
		return Instance{}, fmt.Errorf("实际试启动版本或暂停状态不符")
	}
	// 短暂观察可捕获刚刚就绪便退出的候选，整个窗口仍不接受用户写操作。
	for i := 0; i < 6; i++ {
		if err := instance.CheckHealth(ctx); err != nil {
			return Instance{}, err
		}
		select {
		case <-ctx.Done():
			return Instance{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return instance, nil
}

func launchRecoveredVersion(ctx context.Context, options LaunchOptions) error {
	root, err := ActivePackageRoot(options.PackageRoot, options.DataDir)
	if err != nil {
		return err
	}
	manager, err := updateManager(options)
	if err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	instance, err := launchPausedVersion(ctx, options, root, status.State.ActiveVersion)
	if err != nil {
		return err
	}
	if err := instance.Activate(ctx); err != nil {
		return err
	}
	if _, err := manager.ConfirmRunning(ctx, instance.Version); err != nil {
		return err
	}
	options.PackageRoot = root
	args := []string{"--data-dir", options.DataDir, "--package-root", root}
	if options.NoBrowser {
		args = append(args, "--no-browser")
	} else {
		args = append(args, "--background")
	}
	return startUpdateLauncher(options, args)
}

func recoverRestartJournal(ctx context.Context, options LaunchOptions) error {
	path := filepath.Join(options.DataDir, "updates", restartJournalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return fmt.Errorf("更新恢复记录无效")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var j restartJournal
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if filepath.Dir(j.Work) != filepath.Join(options.DataDir, "updates") || !strings.HasPrefix(filepath.Base(j.Work), "restart-profile-") {
		return fmt.Errorf("更新快照目录无效")
	}
	if j.Phase == "finished" {
		return finishRestartJournal(options.DataDir, j)
	}
	if _, err := PrepareDirectory(j.Work); err != nil {
		return err
	}
	manager, err := updateManager(options)
	if err != nil {
		return err
	}
	instance, runningErr := Running(ctx, options.DataDir)
	if j.Phase == "committing" {
		if runningErr == nil {
			if instance.Version != j.Version {
				return fmt.Errorf("后台已变化，停止更新恢复")
			}
			if err := instance.CheckHealth(ctx); err != nil {
				return err
			}
			if instance.StartupPaused {
				if err := instance.Activate(ctx); err != nil {
					return err
				}
			}
		}
		return finishRestartJournal(options.DataDir, j)
	}
	if runningErr == nil && (instance.Version != j.Version || !instance.StartupPaused) {
		return fmt.Errorf("后台已开放或身份变化，停止恢复旧快照")
	}
	if err := Quit(ctx, options.DataDir); err != nil {
		return err
	}
	lease, err := Acquire(filepath.Join(options.DataDir, "runtime"))
	if err != nil {
		return err
	}
	defer lease.Close()
	if j.Phase == "trial" {
		// 恢复列表只能来自更新前记录的项目配置；先验证所有副本，随后才写入目标。
		registryPath := filepath.Join(j.Work, "absent-workspaces.json")
		for i, entry := range j.Files {
			if entry.Path == filepath.Join(options.DataDir, "workspaces.json") && entry.SHA256 != "" {
				registryPath = filepath.Join(j.Work, fmt.Sprint(i))
				if digest, err := installFileDigest(registryPath); err != nil || digest != entry.SHA256 {
					return fmt.Errorf("工作区恢复快照不完整")
				}
			}
		}
		paths, err := restartProfilePathsFrom(options.DataDir, registryPath)
		if err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, path := range paths {
			allowed[path] = true
		}
		if len(j.Files) != len(paths) {
			return fmt.Errorf("更新恢复清单不完整")
		}
		seen := map[string]bool{}
		for i, entry := range j.Files {
			if !allowed[entry.Path] || seen[entry.Path] || entry.Database != restartDatabasePath(options.DataDir, entry.Path) {
				return fmt.Errorf("快照不属于项目数据库或设置")
			}
			seen[entry.Path] = true
			if entry.SHA256 != "" {
				if digest, err := installFileDigest(filepath.Join(j.Work, fmt.Sprint(i))); err != nil || digest != entry.SHA256 {
					return fmt.Errorf("更新恢复快照不完整")
				}
			}
		}
		status, err := manager.Status(ctx)
		if err != nil {
			return err
		}
		if status.State.ActiveVersion != j.Version && !(status.State.ActiveVersion == j.Previous && status.State.RejectedVersion == j.Version) {
			return fmt.Errorf("活动版本已经变化，停止恢复旧快照")
		}
		for i, entry := range j.Files {
			targets := []string{entry.Path}
			if entry.Database {
				targets = append(targets, entry.Path+"-wal", entry.Path+"-shm")
			}
			for _, target := range targets {
				if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
					return fmt.Errorf("更新恢复目标不是普通文件")
				}
				if target == entry.Path && entry.SHA256 != "" {
					if _, err := copyInstallFile(filepath.Join(j.Work, fmt.Sprint(i)), target); err != nil {
						return err
					}
				} else if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	} else if j.Phase != "preparing" {
		return fmt.Errorf("未知更新恢复阶段")
	}
	if err := manager.FailStartup(ctx, j.Version, j.Previous, "候选启动未完成，已恢复更新前的项目数据库和设置"); err != nil {
		return err
	}
	return finishRestartJournal(options.DataDir, j)
}

// guardUpdateRestart 保护正常启动与中断恢复；独立重启进程持锁期间等待完成。
func guardUpdateRestart(ctx context.Context, options LaunchOptions) (io.Closer, error) {
	dir, err := PrepareDirectory(filepath.Join(options.DataDir, "runtime"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "restart.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("更新重启锁不是普通文件")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = lockInstance(file)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrAlreadyRunning) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("正在完成更新，请稍后重新打开：%w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if _, err := os.Lstat(filepath.Join(options.DataDir, "updates", restartJournalName)); err == nil {
		installationGuard, err := guardInstallation(ctx, options.DataDir)
		if err != nil {
			file.Close()
			return nil, err
		}
		recoverErr := recoverRestartJournal(ctx, options)
		installationGuard.Close()
		if recoverErr != nil {
			file.Close()
			return nil, recoverErr
		}
	} else if !os.IsNotExist(err) {
		file.Close()
		return nil, err
	}
	return file, nil
}
