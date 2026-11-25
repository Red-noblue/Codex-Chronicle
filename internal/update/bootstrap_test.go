// 中文说明：验证首次安装自动建立可回滚基线、旧安装状态迁移，以及重新打开应用不会撤销更新或回滚。
package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRegisterInstallationCreatesBaselineAndPreservesUpdateAndRollback(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	installed := makeBundle(t, "1.0.0", "原始安装")
	if err := manager.RegisterInstallation(t.Context(), installed, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(t.Context())
	if err != nil || status.State.ActiveVersion != "1.0.0" || status.State.InstalledVersion != "1.0.0" || status.State.PreviousVersion != "" || len(status.Versions) != 1 {
		t.Fatalf("首次安装基线错误：%+v %v", status, err)
	}
	plan, err := manager.Prepare(t.Context(), makeBundle(t, "2.0.0", "更新载荷"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(t.Context(), plan, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), installed, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status(t.Context())
	if status.State.ActiveVersion != "2.0.0" || status.State.InstalledVersion != "1.0.0" || status.State.PreviousVersion != "1.0.0" {
		t.Fatalf("重新打开原安装器撤销了更新：%+v", status.State)
	}
	if _, err := manager.Rollback(t.Context(), manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), installed, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status(t.Context())
	if status.State.ActiveVersion != "1.0.0" || status.State.PreviousVersion != "2.0.0" || status.State.InstalledVersion != "1.0.0" {
		t.Fatalf("重新打开安装器破坏了回滚结果：%+v", status.State)
	}
}

func TestRegisterInstallationAdoptsNewInstallerButPreservesLegacyActiveVersion(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := manager.Prepare(t.Context(), makeBundle(t, "2.0.0", "旧流程已激活"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(t.Context(), plan, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), makeBundle(t, "1.0.0", "原安装包"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	status, _ := manager.Status(t.Context())
	if status.State.ActiveVersion != "2.0.0" || status.State.PreviousVersion != "1.0.0" || status.State.InstalledVersion != "1.0.0" {
		t.Fatalf("旧状态迁移改变了已激活版本或缺失回滚基线：%+v", status.State)
	}
	if err := manager.RegisterInstallation(t.Context(), makeBundle(t, "3.0.0", "新安装包"), "3.0.0"); err != nil {
		t.Fatal(err)
	}
	status, _ = manager.Status(t.Context())
	if status.State.ActiveVersion != "3.0.0" || status.State.PreviousVersion != "2.0.0" || status.State.InstalledVersion != "3.0.0" {
		t.Fatalf("新安装器仍被旧指针遮蔽：%+v", status.State)
	}
}

func TestRegisterInstallationRejectsWrongIdentityAndKeepsIndependentCopy(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeBundle(t, "1.0.0", "原安装包")
	if err := manager.RegisterInstallation(t.Context(), bundle, "2.0.0"); err == nil {
		t.Fatal("安装清单与启动器版本不符时被接纳")
	}
	if err := manager.RegisterInstallation(t.Context(), bundle, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "bin", "codex-chronicle"), []byte("来源发生变化"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.VerifyVersionRoot(t.Context(), filepath.Join(manager.Root(), VersionDir, "1.0.0")); err != nil {
		t.Fatalf("原始安装变化影响了可回滚副本：%v", err)
	}
	if err := manager.RegisterInstallation(t.Context(), bundle, "1.0.0"); err == nil {
		t.Fatal("已损坏安装包未被拒绝")
	}
}

func TestRegisterInstallationAllowsEquivalentPackagingButRejectsDifferentPayload(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeBundle(t, "1.0.0", "同一发布载荷")
	plan, err := manager.Prepare(t.Context(), bundle, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(t.Context(), plan, manager.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	manifest := plan.Manifest
	manifest.CreatedAt = manifest.CreatedAt.Add(time.Second)
	if err := WriteManifest(t.Context(), bundle, manifest); err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), bundle, "1.0.0"); err != nil {
		t.Fatalf("同一发布不同打包时间被错误拒绝：%v", err)
	}
	other, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other.Root(), VersionDir, "1.0.0"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := other.RegisterInstallation(t.Context(), bundle, "1.0.0"); err == nil {
		t.Fatal("已损坏的同名版本目录被接纳")
	}
}

func TestRegisterInstallationEnforcesTrustedSignature(t *testing.T) {
	bundle := makeSignedBundle(t)
	for _, trusted := range []bool{true, false} {
		key := readBundlePublicKey(t, bundle)
		if !trusted {
			key = makePublicKey(t)
		}
		manager, err := NewManagerWithOptions(t.TempDir(), ManagerOptions{RequireSignature: true, TrustedPublicKeys: [][]byte{key}})
		if err != nil {
			t.Fatal(err)
		}
		err = manager.RegisterInstallation(t.Context(), bundle, "1.0.0")
		if (err == nil) != trusted {
			t.Fatalf("安装基线未遵守信任根策略：trusted=%v error=%v", trusted, err)
		}
		if !trusted {
			if _, err := os.Stat(filepath.Join(manager.Root(), StateName)); !os.IsNotExist(err) {
				t.Fatal("不受信任的首次安装仍写入了版本状态")
			}
		}
	}
}

func TestInterruptedBaselineCopyIsCleanedWithoutChangingInstalledVersion(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RegisterInstallation(t.Context(), makeBundle(t, "1.0.0", "完整基线"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	interrupted := filepath.Join(manager.Root(), VersionDir, ".baseline-"+strings.Repeat("a", 24))
	if err := os.Mkdir(interrupted, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(interrupted, "partial"), []byte("中断的副本"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(interrupted); !os.IsNotExist(err) {
		t.Fatal("中断的基线副本未被清理")
	}
	if err := manager.VerifyVersionRoot(t.Context(), filepath.Join(manager.Root(), VersionDir, "1.0.0")); err != nil {
		t.Fatalf("清理影响了完整基线：%v", err)
	}
}
