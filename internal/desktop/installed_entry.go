// 中文说明：登记通过验签的安装位置，使固定登录入口尊重卸载与手动覆盖安装，旧版默认位置仅在唯一核验成功时迁移。
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	updatepkg "codex-chronicle.local/internal/update"
)

var errInstalledEntryRemoved = errors.New("应用安装目录已移除，保留用户数据但不再登录启动")

type installedEntry struct {
	PackageRoot      string `json:"package_root"`
	InstalledVersion string `json:"installed_version"`
}

func managedInstalledRoot(dataDir, root string) bool {
	updates := filepath.Join(dataDir, "updates")
	if resolved, err := filepath.EvalSymlinks(updates); err == nil {
		updates = resolved
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return pathsOverlap(updates, root)
}

func installedEntryPath(dataDir string) string {
	return filepath.Join(filepath.Dir(loginLauncherPath(dataDir)), "installed-entry.json")
}

// recordInstalledEntry 只由外部安装载荷通过完整验签之后调用，绝不把更新或预检目录登记为安装位置。
func recordInstalledEntry(dataDir, root string) error {
	root, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return err
	}
	if managedInstalledRoot(dataDir, root) {
		return fmt.Errorf("不能把更新或预检载荷登记为系统安装入口")
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	content, err := os.ReadFile(filepath.Join(root, updatepkg.ManifestName))
	if err != nil {
		return err
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil || manifest.Version == "" {
		return fmt.Errorf("无法记录安装入口的基线版本")
	}
	dir, err := PrepareDirectory(filepath.Dir(installedEntryPath(dataDir)))
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "installed-entry.json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("安装位置记录不是普通文件")
	}
	body, err := json.Marshal(installedEntry{PackageRoot: root, InstalledVersion: manifest.Version})
	if err != nil {
		return err
	}
	return writeInstallFile(path, body)
}

func readInstalledEntry(dataDir string) (installedEntry, error) {
	path := installedEntryPath(dataDir)
	info, err := os.Lstat(path)
	if err != nil {
		return installedEntry{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<10 {
		return installedEntry{}, fmt.Errorf("安装位置记录无效")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return installedEntry{}, err
	}
	var entry installedEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return installedEntry{}, err
	}
	if entry.InstalledVersion == "" || !filepath.IsAbs(entry.PackageRoot) || filepath.Clean(entry.PackageRoot) != entry.PackageRoot || managedInstalledRoot(dataDir, entry.PackageRoot) {
		return installedEntry{}, fmt.Errorf("安装位置不能指向更新目录或相对路径")
	}
	return entry, nil
}

// discoverLegacyInstalledEntry 迁移没有位置记录的旧安装；非标准便携位置保持兼容，多个匹配时不猜测。
func discoverLegacyInstalledEntry(ctx context.Context, options LaunchOptions) error {
	entry, readErr := readInstalledEntry(options.DataDir)
	if readErr == nil {
		if _, err := os.Lstat(filepath.Join(entry.PackageRoot, updatepkg.ManifestName)); !os.IsNotExist(err) {
			return err
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	manager, err := updateManager(options)
	if err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil || status.State.InstalledVersion == "" {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"/Applications/Codex Chronicle.app/Contents/Resources/app", filepath.Join(home, "Applications", "Codex Chronicle.app", "Contents", "Resources", "app")}
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(local) {
			candidates = []string{filepath.Join(local, "Programs", "Codex Chronicle")}
		}
	case "linux":
		candidates = []string{"/usr/lib/codex-chronicle"}
	}
	var found []string
	for _, root := range candidates {
		if manager.VerifyVersionRoot(ctx, root) != nil {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, updatepkg.ManifestName))
		if err != nil {
			continue
		}
		var manifest updatepkg.Manifest
		if json.Unmarshal(body, &manifest) == nil && manifest.Version == status.State.InstalledVersion {
			found = append(found, root)
		}
	}
	if len(found) == 1 {
		return recordInstalledEntry(options.DataDir, found[0])
	}
	return nil
}

func loginPackageRoot(ctx context.Context, manager *updatepkg.Manager, dataDir, active string) (string, error) {
	entry, err := readInstalledEntry(dataDir)
	if os.IsNotExist(err) {
		return active, nil // 老版非标准便携安装保留兼容，下一次从新安装入口打开时补齐位置。
	}
	if err != nil {
		return "", err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return "", err
	}
	if entry.InstalledVersion != status.State.InstalledVersion {
		return active, nil // 安装指针已提交但位置记录尚未同步时，不用旧记录撤销新安装。
	}
	if _, err := os.Lstat(filepath.Join(entry.PackageRoot, updatepkg.ManifestName)); os.IsNotExist(err) {
		return "", errInstalledEntryRemoved
	}
	if err := manager.VerifyVersionRoot(ctx, entry.PackageRoot); err != nil {
		return "", fmt.Errorf("原安装入口未通过验签：%w", err)
	}
	body, err := os.ReadFile(filepath.Join(entry.PackageRoot, updatepkg.ManifestName))
	if err != nil {
		return "", err
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", err
	}
	if manifest.Version != status.State.InstalledVersion {
		return entry.PackageRoot, nil // 用户已手动覆盖安装，由新安装器登记其版本，不撤销人工回滚。
	}
	return active, nil
}
