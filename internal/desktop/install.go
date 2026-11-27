// 中文说明：持久化 macOS 安装事务；未开放操作的安装恢复旧应用和设置，开始开放后只完成提交，避免覆盖用户新数据。
package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const installMarker = ".install-in-progress"
const appEntry = "Contents/MacOS/Codex Chronicle"

var installProfileFiles = []string{"archive.db", "workspaces.json", "launcher.json", "auth.json", "trash-settings.json", "github-settings.json", "github-archive-selection.json", "updates/install-state.json"}

type installJournal struct {
	Format         int               `json:"format"`
	Target         string            `json:"target"`
	DataDir        string            `json:"data_dir"`
	Work           string            `json:"work"`
	Phase          string            `json:"phase"`
	Version        string            `json:"version"`
	CandidateHash  string            `json:"candidate_hash"`
	PreviousHash   string            `json:"previous_hash,omitempty"`
	Snapshot       bool              `json:"snapshot"`
	ProfileDigests map[string]string `json:"profile_digests,omitempty"`
}

// guardInstallation 与安装器使用同一内核锁，普通启动不能与恢复、快照或替换应用并发。
func guardInstallation(ctx context.Context, data string) (io.Closer, error) {
	if runtime.GOOS != "darwin" {
		return io.NopCloser(strings.NewReader("")), nil
	}
	lock, err := installLock(data)
	if err != nil {
		return nil, err
	}
	if _, err := recoverInstallation(ctx, data); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

func installLock(data string) (*os.File, error) {
	dir, err := PrepareDirectory(filepath.Join(data, "runtime"))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "install.lock")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("安装锁不是普通文件")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockInstance(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("已有安装、恢复或启动任务正在使用此用户目录：%w", err)
	}
	return file, nil
}

// InstallMacApp 安装签名完整的应用。只替换本项目应用和指定用户目录中的项目设置，不接触对话源。
func InstallMacApp(ctx context.Context, source, target, data string, output io.Writer) (resultErr error) {
	return installMacApp(ctx, source, target, data, output, func(string) {})
}

func installMacApp(ctx context.Context, source, target, data string, output io.Writer, checkpoint func(string)) (resultErr error) {
	if runtime.GOOS != "darwin" {
		return errors.New("完整 .app 安装仅支持 macOS")
	}
	for _, path := range []string{source, target} {
		if !filepath.IsAbs(path) || filepath.Ext(path) != ".app" {
			return errors.New("应用来源和目标必须为 .app 绝对路径")
		}
	}
	source = filepath.Clean(source)
	if info, err := os.Lstat(source); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("应用来源必须是普通应用目录")
	}
	parent, err := PrepareDirectory(filepath.Dir(target))
	if err != nil {
		return err
	}
	target = filepath.Join(parent, filepath.Base(target))
	data, err = PrepareDirectory(data)
	if err != nil {
		return err
	}
	if pathsOverlap(source, target) || pathsOverlap(source, data) || pathsOverlap(target, data) {
		return errors.New("应用来源、目标和用户数据目录不能相同或互相包含")
	}
	candidateHash, err := appDigest(ctx, source)
	if err != nil {
		return err
	}
	version, err := exec.CommandContext(ctx, filepath.Join(source, appEntry), "--version").Output()
	if err != nil {
		return fmt.Errorf("无法读取候选版本：%w", err)
	}
	lock, err := installLock(data)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			recoveryCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			journal, recoveryErr := recoverInstallation(recoveryCtx, data)
			if recoveryErr != nil {
				resultErr = fmt.Errorf("%w；恢复未完成，请保留安装材料：%v", resultErr, recoveryErr)
			} else if journal != nil && journal.Phase == "rolled_back" {
				fmt.Fprintln(output, "安装检查失败，已恢复旧应用和升级前设置；未改动对话源或归档。")
			}
			_ = lock.Close()
			if recoveryErr == nil && journal != nil && journal.PreviousHash != "" && os.Getenv("CHRONICLE_NO_OPEN") != "1" {
				if err := runInstallCommand(recoveryCtx, output, "/usr/bin/open", "-g", journal.Target, "--args", "--background", "--data-dir", data); err != nil {
					resultErr = fmt.Errorf("%w；恢复的应用未能重新打开，请从应用程序中打开：%v", resultErr, err)
				}
			}
			return
		}
		_ = lock.Close()
	}()
	if _, err := recoverInstallation(ctx, data); err != nil {
		return err
	}
	previousHash := ""
	if _, err := os.Lstat(target); err == nil {
		previousHash, err = appDigest(ctx, target)
		if err != nil {
			return fmt.Errorf("安装目标不是完整的本项目应用，拒绝覆盖：%w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	work, err := os.MkdirTemp(parent, ".chronicle-install-")
	if err != nil {
		return err
	}
	journal := &installJournal{Format: 1, Target: target, DataDir: data, Work: work, Phase: "preparing", Version: strings.TrimSpace(string(version)), CandidateHash: candidateHash, PreviousHash: previousHash}
	if err := saveInstallJournal(journal); err != nil {
		_ = os.RemoveAll(work)
		return err
	}
	fmt.Fprintln(output, "正在等待现有后台完成数据任务…")
	if err := Quit(ctx, data); err != nil {
		return err
	}
	if err := WaitTrayExit(ctx, data); err != nil {
		return err
	}
	fmt.Fprintln(output, "正在使用隔离副本检查新版本和项目数据库…")
	if err := runInstallCommand(ctx, output, filepath.Join(source, appEntry), "--verify", "--data-dir", data); err != nil {
		return err
	}
	if err := snapshotInstallProfile(journal); err != nil {
		return err
	}
	checkpoint("snapshot_saved")
	if err := runInstallCommand(ctx, output, "/usr/bin/ditto", source, filepath.Join(work, "candidate.app")); err != nil {
		return err
	}
	if digest, err := appDigest(ctx, filepath.Join(work, "candidate.app")); err != nil || digest != candidateHash {
		return fmt.Errorf("候选应用复制后校验失败：%v", err)
	}
	if err := syncInstallTree(filepath.Join(work, "candidate.app")); err != nil {
		return err
	}
	journal.Phase = "replacing"
	if err := saveInstallJournal(journal); err != nil {
		return err
	}
	if previousHash != "" {
		if digest, err := appDigest(ctx, target); err != nil || digest != previousHash {
			return errors.New("旧应用在安装期间发生变化，停止替换")
		}
		if err := installRename(target, filepath.Join(work, "previous.app")); err != nil {
			return err
		}
		checkpoint("previous_moved")
	}
	if err := installRename(filepath.Join(work, "candidate.app"), target); err != nil {
		return err
	}
	checkpoint("candidate_installed")
	journal.Phase = "checking"
	if err := saveInstallJournal(journal); err != nil {
		return err
	}
	fmt.Fprintln(output, "正在检查已安装版本，检查期间暂停接收操作…")
	if err := runInstallCommand(ctx, output, filepath.Join(target, appEntry), "--startup-check", "--no-browser", "--data-dir", data); err != nil {
		return err
	}
	// 必须先持久化提交决定，再开放请求。此后即使强制中断，也不允许用旧快照覆盖用户新数据。
	journal.Phase = "committing"
	if err := saveInstallJournal(journal); err != nil {
		return err
	}
	checkpoint("commit_recorded")
	if err := runInstallCommand(ctx, output, filepath.Join(target, appEntry), "--activate", "--data-dir", data); err != nil {
		return err
	}
	checkpoint("activated")
	// 安装检查进程由终端启动；提交后结束它，再由外层通过 LaunchServices 启动正式应用。
	fmt.Fprintln(output, "正在结束安装检查后台，准备交由系统启动应用…")
	if err := Quit(ctx, data); err != nil {
		return fmt.Errorf("应用已安装，但安装检查后台尚未退出：%w", err)
	}
	checkpoint("check_stopped")
	if _, err := recoverInstallation(ctx, data); err != nil {
		return err
	}
	fmt.Fprintf(output, "安装完成：%s\n", target)
	return nil
}

func snapshotInstallProfile(journal *installJournal) error {
	lease, err := Acquire(filepath.Join(journal.DataDir, "runtime"))
	if err != nil {
		return fmt.Errorf("快照前无法独占项目数据库：%w", err)
	}
	defer lease.Close()
	if info, err := os.Stat(filepath.Join(journal.DataDir, "archive.db-wal")); err == nil && info.Size() != 0 {
		return errors.New("项目数据库 WAL 尚未合并，停止安装")
	}
	backup := filepath.Join(journal.Work, "profile")
	if err := os.Mkdir(backup, 0o700); err != nil {
		return err
	}
	journal.ProfileDigests = make(map[string]string)
	for _, name := range installProfileFiles {
		path := filepath.Join(journal.DataDir, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(backup, name)), 0o700); err != nil {
			return err
		}
		digest, err := copyInstallFile(path, filepath.Join(backup, name))
		if err != nil {
			return err
		}
		journal.ProfileDigests[name] = digest
	}
	if err := syncInstallDir(backup); err != nil {
		return err
	}
	journal.Snapshot = true
	return saveInstallJournal(journal)
}

func recoverInstallation(ctx context.Context, data string) (*installJournal, error) {
	path := filepath.Join(data, installMarker)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return nil, errors.New("安装标记不是可恢复的事务记录；请保留现场检查，未改动数据")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var journal installJournal
	if err := json.Unmarshal(content, &journal); err != nil {
		return nil, fmt.Errorf("安装记录无法解析，未改动数据：%w", err)
	}
	if err := validateInstallJournal(&journal, data); err != nil {
		return nil, errors.New("安装事务的路径或身份不完整，拒绝自动恢复")
	}
	switch journal.Phase {
	case "committing":
		if digest, err := appDigest(ctx, journal.Target); err != nil || digest != journal.CandidateHash {
			return nil, errors.New("已经决定提交的候选应用发生变化，保留现场，禁止回退用户数据")
		}
		instance, err := Running(ctx, data)
		if err == nil {
			if instance.Version != journal.Version {
				return nil, errors.New("提交恢复遇到不同版本的后台，未改动数据")
			}
			if instance.StartupPaused {
				if err := instance.CheckHealth(ctx); err != nil {
					return nil, err
				}
				if err := instance.Activate(ctx); err != nil {
					return nil, err
				}
			}
		} else {
			lease, lockErr := Acquire(filepath.Join(data, "runtime"))
			if lockErr != nil {
				return nil, fmt.Errorf("无法核对提交后台，未改动数据：%w", lockErr)
			}
			_ = lease.Close()
		}
	case "preparing", "replacing", "checking":
		if err := Quit(ctx, data); err != nil {
			return nil, err
		}
		lease, err := Acquire(filepath.Join(data, "runtime"))
		if err != nil {
			return nil, fmt.Errorf("恢复前无法独占项目数据库：%w", err)
		}
		err = restoreInstallation(ctx, &journal)
		_ = lease.Close()
		if err != nil {
			return nil, err
		}
		journal.Phase = "rolled_back"
		if err := saveInstallJournal(&journal); err != nil {
			return nil, err
		}
	case "rolled_back":
		if journal.PreviousHash != "" {
			if digest, err := appDigest(ctx, journal.Target); err != nil || digest != journal.PreviousHash {
				return nil, errors.New("恢复后的旧应用发生变化，保留安装材料")
			}
		}
	default:
		return nil, fmt.Errorf("未知安装阶段 %q，未改动数据", journal.Phase)
	}
	if err := os.RemoveAll(journal.Work); err != nil {
		return nil, err
	}
	if err := syncInstallDir(filepath.Dir(journal.Work)); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		return nil, err
	}
	return &journal, syncInstallDir(data)
}

func validateInstallJournal(journal *installJournal, data string) error {
	if journal.Format != 1 || journal.DataDir != data || !filepath.IsAbs(journal.Target) || filepath.Clean(journal.Target) != journal.Target || filepath.Ext(journal.Target) != ".app" ||
		!filepath.IsAbs(journal.Work) || filepath.Clean(journal.Work) != journal.Work || filepath.Dir(journal.Work) != filepath.Dir(journal.Target) ||
		!strings.HasPrefix(filepath.Base(journal.Work), ".chronicle-install-") || pathsOverlap(journal.Work, data) || pathsOverlap(journal.Target, data) || journal.Version == "" {
		return errors.New("安装路径或版本无效")
	}
	for _, path := range []string{journal.Work, journal.Target} {
		for current := path; ; current = filepath.Dir(current) {
			if strings.EqualFold(filepath.Base(current), ".codex") {
				return errors.New("安装恢复不能位于 .codex 内")
			}
			if filepath.Dir(current) == current {
				break
			}
		}
		if info, err := os.Lstat(path); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return errors.New("安装恢复遇到非目录或符号链接")
		}
	}
	parent := filepath.Dir(journal.Target)
	if canonical, err := filepath.EvalSymlinks(parent); err != nil || canonical != parent {
		return errors.New("安装父目录发生变化")
	}
	digests := []string{journal.CandidateHash}
	if journal.PreviousHash != "" {
		digests = append(digests, journal.PreviousHash)
	}
	for name, digest := range journal.ProfileDigests {
		allowed := false
		for _, expected := range installProfileFiles {
			allowed = allowed || name == expected
		}
		if !allowed {
			return errors.New("安装快照包含未知对象")
		}
		digests = append(digests, digest)
	}
	for _, digest := range digests {
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			return errors.New("安装记录摘要无效")
		}
	}
	return nil
}

func restoreInstallation(ctx context.Context, journal *installJournal) error {
	backup := filepath.Join(journal.Work, "profile")
	// 先核对全部快照，再修改任何目标，防止用不完整快照覆盖原数据。
	if journal.Snapshot {
		for _, name := range installProfileFiles {
			if expected, exists := journal.ProfileDigests[name]; exists {
				digest, err := installFileDigest(filepath.Join(backup, name))
				if err != nil || digest != expected {
					return fmt.Errorf("安装快照 %s 不完整，停止恢复", name)
				}
			}
		}
	}
	previous := filepath.Join(journal.Work, "previous.app")
	_, previousErr := os.Lstat(previous)
	if previousErr == nil {
		if digest, err := appDigest(ctx, previous); err != nil || digest != journal.PreviousHash || journal.PreviousHash == "" {
			return errors.New("旧应用恢复副本不完整，停止恢复")
		}
	} else if !os.IsNotExist(previousErr) {
		return previousErr
	}
	if _, err := os.Lstat(journal.Target); err == nil {
		digest, err := appDigest(ctx, journal.Target)
		if err != nil {
			return err
		}
		if digest == journal.PreviousHash && os.IsNotExist(previousErr) {
			// 替换尚未开始，或者此前一次恢复已经归还旧应用。
		} else if digest == journal.CandidateHash {
			if err := installRename(journal.Target, filepath.Join(journal.Work, "discard.app")); err != nil {
				return err
			}
		} else {
			return errors.New("安装目标发生未知变化，拒绝覆盖")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if previousErr == nil {
		if err := installRename(previous, journal.Target); err != nil {
			return err
		}
	}
	if journal.PreviousHash != "" {
		if digest, err := appDigest(ctx, journal.Target); err != nil || digest != journal.PreviousHash {
			return errors.New("无法完整归还旧应用，保留安装材料")
		}
	}
	if journal.Snapshot {
		for _, name := range append(append([]string{}, installProfileFiles...), "archive.db-wal", "archive.db-shm") {
			target := filepath.Join(journal.DataDir, name)
			if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
				return fmt.Errorf("恢复目标 %s 不是普通文件", name)
			}
			if _, exists := journal.ProfileDigests[name]; exists {
				if _, err := copyInstallFile(filepath.Join(backup, name), target); err != nil {
					return err
				}
			} else if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return syncInstallDir(journal.DataDir)
}

func saveInstallJournal(journal *installJournal) error {
	content, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeInstallFile(filepath.Join(journal.DataDir, installMarker), content)
}

func writeInstallFile(path string, content []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".install-write-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceInstallFile(file.Name(), path)
}

func copyInstallFile(source, target string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("安装快照只接受普通文件：%s", source)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(target), ".install-copy-")
	if err != nil {
		return "", err
	}
	defer os.Remove(output.Name())
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(output, hash), input)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), replaceInstallFile(output.Name(), target)
}

// replaceInstallFile 供各平台更新快照使用；Windows 用原子替换与写穿，Unix 另同步目录。
func replaceInstallFile(source, target string) error {
	if err := replaceStartupFile(source, target); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := syncInstallDir(filepath.Dir(source)); err != nil {
		return err
	}
	return syncInstallDir(filepath.Dir(target))
}

func installFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("校验目标不是普通文件：%s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func appDigest(ctx context.Context, root string) (string, error) {
	if info, err := os.Lstat(root); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("应用路径不是普通目录")
	}
	if err := runInstallCommand(ctx, io.Discard, "/usr/bin/codesign", "--verify", "--deep", "--strict", root); err != nil {
		return "", err
	}
	identifier, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleIdentifier", "raw", "-o", "-", filepath.Join(root, "Contents/Info.plist")).Output()
	if err != nil || strings.TrimSpace(string(identifier)) != "io.github.red-noblue.codex-chronicle" {
		return "", errors.New("应用标识不匹配")
	}
	hash := sha256.New()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			fmt.Fprintf(hash, "directory:%s\x00", relative)
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("应用包含特殊文件：%s", relative)
		}
		digest, err := installFileDigest(path)
		if err == nil {
			fmt.Fprintf(hash, "file:%s:%o:%s\x00", relative, info.Mode().Perm(), digest)
		}
		return err
	})
	return hex.EncodeToString(hash.Sum(nil)), err
}

func syncInstallTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return syncInstallDir(path)
	})
}

func syncInstallDir(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func installRename(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	if err := syncInstallDir(filepath.Dir(source)); err != nil {
		return err
	}
	return syncInstallDir(filepath.Dir(target))
}

func pathsOverlap(left, right string) bool {
	return left == right || strings.HasPrefix(left, right+string(os.PathSeparator)) || strings.HasPrefix(right, left+string(os.PathSeparator))
}

func runInstallCommand(ctx context.Context, output io.Writer, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		return fmt.Errorf("安装命令 %s 失败：%w", filepath.Base(name), err)
	}
	return nil
}
