// 中文说明：验证固定登录入口尊重安装位置、卸载和手动覆盖安装，所有位置与签名材料均为隔离样本。
package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	updatepkg "codex-chronicle.local/internal/update"
)

func TestInstalledEntryPreservesUpdatesAndStopsAfterUninstall(t *testing.T) {
	data, bundle := loginFixture(t)
	installed := bundle("1.0.0", "安装器 1")
	manager, err := updateManager(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), installed.PackageRoot, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := recordInstalledEntry(data, installed.PackageRoot); err != nil {
		t.Fatal(err)
	}
	updated := bundle("2.0.0", "启动器 2")
	plan, err := manager.Prepare(t.Context(), updated.PackageRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(t.Context(), plan, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(data, "updates", "versions", "2.0.0")
	if root, err := loginPackageRoot(t.Context(), manager, data, active); err != nil || root != active {
		t.Fatalf("原安装位置覆盖了已激活更新：%s %v", root, err)
	}
	if err := PrepareLoginLauncher(t.Context(), updated); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(installed.PackageRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := loginPackageRoot(t.Context(), manager, data, active); !errors.Is(err, errInstalledEntryRemoved) {
		t.Fatalf("卸载后仍准备从保留缓存启动：%v", err)
	}
	if err := LaunchActiveFromLogin(t.Context(), loginLauncherPath(data), data, true); err != nil {
		t.Fatalf("卸载后的登录入口不应弹出故障：%v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "runtime", "instance.json")); !os.IsNotExist(err) {
		t.Fatal("卸载后重新启动了后台")
	}
	if _, err := os.Stat(filepath.Join(active, updatepkg.ManifestName)); err != nil {
		t.Fatalf("卸载联动删除了保留的用户更新数据：%v", err)
	}
}

func TestInstalledEntryRecognizesManualReplacementAndRejectsTamper(t *testing.T) {
	data, bundle := loginFixture(t)
	installed := bundle("1.0.0", "安装器 1")
	manager, err := updateManager(installed)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), installed.PackageRoot, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := recordInstalledEntry(data, installed.PackageRoot); err != nil {
		t.Fatal(err)
	}
	replacement := bundle("3.0.0", "安装器 3")
	for _, name := range []string{filepath.Join("bin", ExecutableName("codex-chronicle")), filepath.Join("bin", ExecutableName("codex-chronicle-desktop")), filepath.Join("update", "trusted-public-key"), updatepkg.ManifestName} {
		body, err := os.ReadFile(filepath.Join(replacement.PackageRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(installed.PackageRoot, name), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	active := filepath.Join(data, "updates", "versions", "1.0.0")
	expectedRoot, err := filepath.EvalSymlinks(installed.PackageRoot)
	if err != nil {
		t.Fatal(err)
	}
	if root, err := loginPackageRoot(t.Context(), manager, data, active); err != nil || root != expectedRoot {
		t.Fatalf("没有交接用户手动覆盖的新安装器：%s %v", root, err)
	}
	if err := os.WriteFile(filepath.Join(installed.PackageRoot, "bin", ExecutableName("codex-chronicle-desktop")), []byte("被篡改的安装器"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := loginPackageRoot(t.Context(), manager, data, active); err == nil || errors.Is(err, errInstalledEntryRemoved) {
		t.Fatalf("把被篡改的程序当成可执行或已卸载：%v", err)
	}
	if err := recordInstalledEntry(data, active); err == nil {
		t.Fatal("把会被清理的更新目录登记成了系统安装位置")
	}
}
