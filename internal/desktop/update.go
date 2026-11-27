// 中文说明：在启动前登记已签名安装基线、验证活动载荷，并把浏览器和托盘入口交给同一版本的启动器。
package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	updatepkg "codex-chronicle.local/internal/update"
)

func resolveUpdatePackage(ctx context.Context, options LaunchOptions) (LaunchOptions, error) {
	keyPath := filepath.Join(options.PackageRoot, "update", "trusted-public-key")
	if _, err := os.Lstat(keyPath); os.IsNotExist(err) {
		return options, nil // 开发包没有发布信任根，不自动生成安装状态。
	} else if err != nil {
		return options, err
	}
	keys, err := updatepkg.LoadTrustedPublicKeys(keyPath)
	if err != nil {
		return options, err
	}
	manager, err := updatepkg.NewManagerWithOptions(filepath.Join(options.DataDir, "updates"), updatepkg.ManagerOptions{
		RequireSignature: true, TrustedPublicKeys: keys, Online: updatepkg.OnlineConfig{FeedURL: options.UpdateFeedURL},
	})
	if err != nil {
		return options, err
	}
	if err := manager.Recover(ctx); err != nil {
		return options, err
	}
	// 活动版本和隔离预检的 staging 不是外部安装器，不能重新登记成新的安装基线。
	if !pathsOverlap(filepath.Join(manager.Root(), updatepkg.VersionDir), options.PackageRoot) {
		if err := manager.RegisterInstallation(ctx, options.PackageRoot, options.ExpectedVersion); err != nil {
			return options, err
		}
		if !options.StartupCheck {
			if err := recordInstalledEntry(options.DataDir, options.PackageRoot); err != nil {
				return options, err
			}
		}
	}
	if !options.StartupCheck {
		options.PackageRoot, err = ActivePackageRoot(options.PackageRoot, options.DataDir)
		if err != nil {
			return options, err
		}
	}
	if err := manager.VerifyVersionRoot(ctx, options.PackageRoot); err != nil {
		return options, err
	}
	content, err := os.ReadFile(filepath.Join(options.PackageRoot, updatepkg.ManifestName))
	if err != nil {
		return options, err
	}
	var manifest updatepkg.Manifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return options, err
	}
	if options.StartupCheck && options.ExpectedVersion != "" && manifest.Version != options.ExpectedVersion {
		return options, fmt.Errorf("安装候选版本不符：%s", manifest.Version)
	}
	options.ExpectedVersion = manifest.Version
	return options, nil
}

// UpdatedLauncher 返回应接管本次启动的已验签入口；已有后台时保持当前实例，不暗中重启。
func UpdatedLauncher(ctx context.Context, options LaunchOptions) (string, string, error) {
	data, err := PrepareDirectory(options.DataDir)
	if err != nil {
		return "", "", err
	}
	options.DataDir = data
	restartGuard, err := guardUpdateRestart(ctx, options)
	if err != nil {
		return "", "", err
	}
	defer restartGuard.Close()
	_, markerErr := os.Lstat(filepath.Join(data, installMarker))
	recoveringInstallation := runtime.GOOS == "darwin" && markerErr == nil
	guard, err := guardInstallation(ctx, data)
	if err != nil {
		return "", "", err
	}
	defer guard.Close()
	if recoveringInstallation {
		// 恢复可能已经替换当前进程的磁盘文件；重新执行恢复后的入口，不能继续使用内存中的旧版本与渠道。
		return filepath.Join(options.PackageRoot, "bin", ExecutableName("codex-chronicle-desktop")), options.PackageRoot, nil
	}
	if _, err := Running(ctx, data); err == nil {
		return "", "", nil
	}
	resolved, err := resolveUpdatePackage(ctx, options)
	if err != nil {
		return "", "", err
	}
	if resolved.PackageRoot == options.PackageRoot {
		return "", "", nil
	}
	return filepath.Join(resolved.PackageRoot, "bin", ExecutableName("codex-chronicle-desktop")), resolved.PackageRoot, nil
}
