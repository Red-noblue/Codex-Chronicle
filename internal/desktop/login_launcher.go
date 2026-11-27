// 中文说明：为登录启动保留固定的验签入口，自动迁移已开启的旧版本登录项，不改变用户的启停选择。
package desktop

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	updatepkg "codex-chronicle.local/internal/update"
)

var legacyLoginVersion = regexp.MustCompile(`[\\/]updates[\\/]versions[\\/]([0-9][0-9A-Za-z._+-]{0,100})[\\/]`)

func loginLauncherPath(dataDir string) string {
	if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
		dataDir = resolved
	}
	return filepath.Join(dataDir, "desktop-launcher", ExecutableName("codex-chronicle-desktop"))
}

// LoginEntry 保留开发包的原入口；签名安装版的登录项始终指向清理范围之外的固定入口。
func LoginEntry(executable, dataDir, packageRoot string) (string, string) {
	if _, err := os.Stat(filepath.Join(packageRoot, "update", "trusted-public-key")); err == nil || executable == loginLauncherPath(dataDir) {
		return loginLauncherPath(dataDir), ""
	}
	return executable, packageRoot
}

func loginArguments(executable, dataDir, packageRoot string) []string {
	arguments := []string{executable, "--background", "--data-dir", dataDir}
	if executable == loginLauncherPath(dataDir) {
		if resolved, err := filepath.EvalSymlinks(dataDir); err == nil {
			arguments[3] = resolved
		}
		return append(arguments, "--launch-active")
	}
	return append(arguments, "--package-root", packageRoot)
}

// matchesLoginEntry 只接受本程序完整生成的命令；旧路径可以已被清理，其他资料目录和额外参数不会被认领。
func matchesLoginEntry(saved string, dataDir string, render func(string, string) (string, error), executable, packageRoot string) (bool, error) {
	expected, err := render(executable, packageRoot)
	if err != nil || saved == expected {
		return saved == expected && err == nil, err
	}
	for _, match := range legacyLoginVersion.FindAllStringSubmatch(saved, -1) {
		root := filepath.Join(dataDir, "updates", "versions", match[1])
		command, err := render(filepath.Join(root, "bin", ExecutableName("codex-chronicle-desktop")), root)
		if err != nil {
			return false, err
		}
		if saved == command {
			return true, nil
		}
	}
	return false, nil
}

// PrepareLoginLauncher 仅复制签名清单中核验过的启动器；候选健康检查通过后、旧版本清理之前调用。
func PrepareLoginLauncher(ctx context.Context, options LaunchOptions) (err error) {
	keyPath := filepath.Join(options.PackageRoot, "update", "trusted-public-key")
	if _, err := os.Lstat(keyPath); os.IsNotExist(err) {
		return nil
	}
	finish := updatepkg.BeginDiagnostic(filepath.Join(options.DataDir, "updates"), "login_launcher", options.ExpectedVersion)
	defer func() { finish(err) }()
	keys, err := updatepkg.LoadTrustedPublicKeys(keyPath)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(options.PackageRoot, updatepkg.ManifestName)
	info, err := os.Lstat(manifestPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return fmt.Errorf("固定登录启动入口的签名清单不可用")
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return err
	}
	if err := updatepkg.VerifyManifestSignature(manifest, keys); err != nil {
		return err
	}
	if options.ExpectedVersion != "" && manifest.Version != options.ExpectedVersion {
		return fmt.Errorf("固定登录启动入口与当前启动器版本不符")
	}
	target := loginLauncherPath(options.DataDir)
	dir, err := PrepareDirectory(filepath.Dir(target))
	if err != nil {
		return err
	}
	lockPath := filepath.Join(dir, "launcher.lock")
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("固定登录启动入口的锁不是普通文件")
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := lockInstance(lock); err != nil {
		return err
	}
	var keyText strings.Builder
	for _, key := range keys {
		fmt.Fprintln(&keyText, base64.StdEncoding.EncodeToString(key))
	}
	trusted := filepath.Join(dir, "trusted-public-key")
	if info, err := os.Lstat(trusted); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("固定登录启动入口的信任根不是普通文件")
	}
	if current, err := os.ReadFile(trusted); err == nil {
		if info, err := os.Lstat(trusted); err != nil || !info.Mode().IsRegular() || !bytes.Equal(current, []byte(keyText.String())) {
			return fmt.Errorf("固定登录启动入口的信任根已变化，拒绝替换")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if err := writeInstallFile(trusted, []byte(keyText.String())); err != nil {
		return err
	}
	name := filepath.ToSlash(filepath.Join("bin", ExecutableName("codex-chronicle-desktop")))
	found := false
	for _, file := range manifest.Files {
		if file.Path != name {
			continue
		}
		if err := copyLoginLauncher(ctx, filepath.Join(options.PackageRoot, filepath.FromSlash(name)), target, file); err != nil {
			return err
		}
		found = true
		break
	}
	if !found {
		return fmt.Errorf("签名清单缺少桌面启动器")
	}
	enabled, err := AutostartEnabled(target, options.DataDir, "")
	if err != nil || !enabled {
		return err
	}
	return SetAutostart(target, options.DataDir, "", true)
}

func copyLoginLauncher(ctx context.Context, source, target string, entry updatepkg.FileEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current, err := installFileDigest(target); err == nil && current == entry.SHA256 {
		return os.Chmod(target, 0o700)
	} else if info, err := os.Lstat(target); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("固定登录启动入口不是普通文件")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.SizeBytes {
		return fmt.Errorf("桌面启动器不是与签名清单一致的普通文件")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.CreateTemp(filepath.Dir(target), ".login-launcher-")
	if err != nil {
		return err
	}
	defer os.Remove(output.Name())
	defer output.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, entry.SizeBytes+1))
	if err != nil {
		return err
	}
	if n != entry.SizeBytes || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
		return fmt.Errorf("桌面启动器摘要不符，保留原登录启动入口")
	}
	if err := output.Chmod(0o700); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return replaceInstallFile(output.Name(), target)
}

// LaunchActiveFromLogin 固定入口只校验并转交当前载荷，立即退出，不长期占用待维护的启动器文件。
func LaunchActiveFromLogin(ctx context.Context, executable, dataDir string, noBrowser bool) error {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	executable = resolved
	data, err := PrepareDirectory(dataDir)
	if err != nil {
		return err
	}
	dataDir = data
	if executable != loginLauncherPath(dataDir) {
		return fmt.Errorf("登录启动必须使用当前用户的固定入口")
	}
	keys, err := updatepkg.LoadTrustedPublicKeys(filepath.Join(filepath.Dir(executable), "trusted-public-key"))
	if err != nil {
		return err
	}
	manager, err := updatepkg.NewManagerWithOptions(filepath.Join(dataDir, "updates"), updatepkg.ManagerOptions{RequireSignature: true, TrustedPublicKeys: keys})
	if err != nil {
		return err
	}
	status, err := manager.Status(ctx)
	if err != nil {
		return err
	}
	if status.State.ActiveVersion == "" {
		return fmt.Errorf("尚无可用的活动版本，请从已安装的应用重新打开")
	}
	root, err := ActivePackageRoot("", dataDir)
	if err != nil {
		return err
	}
	if err := manager.VerifyVersionRoot(ctx, root); err != nil {
		return fmt.Errorf("当前版本未通过登录启动验签：%w", err)
	}
	arguments := []string{"--background", "--data-dir", dataDir, "--package-root", root}
	if noBrowser {
		arguments = append(arguments, "--no-browser")
	}
	return startUpdateLauncher(LaunchOptions{DataDir: dataDir, PackageRoot: root}, arguments)
}
