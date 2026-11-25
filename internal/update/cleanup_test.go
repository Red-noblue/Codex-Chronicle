// 中文说明：验证更新成功启动后的缓存淘汰、回滚与安装基线保留，以及待确认计划、下载租约和用户外部包的保护。
package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanupRequiresRunningVersionAndPreservesRollbackAndBaseline(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	baseline := makeBundle(t, "1.0.0", "安装基线")
	if err := m.RegisterInstallation(t.Context(), baseline, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2.0.0", "3.0.0", "4.0.0"} {
		plan, err := m.Prepare(t.Context(), makeBundle(t, version, version), false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
			t.Fatal(err)
		}
	}
	obsolete := filepath.Join(m.root, VersionDir, "2.0.0")
	if _, err := m.ConfirmRunning(t.Context(), "3.0.0"); err == nil {
		t.Fatal("旧后台触发了清理")
	}
	if _, err := os.Stat(obsolete); err != nil {
		t.Fatal("新版本未启动就删除了历史版本")
	}
	result, err := m.ConfirmRunning(t.Context(), "4.0.0")
	if err != nil || result.RemovedItems != 1 || result.FreedBytes == 0 {
		t.Fatalf("清理结果错误：%+v %v", result, err)
	}
	if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
		t.Fatal("未清理不再使用的历史版本")
	}
	for _, version := range []string{"1.0.0", "3.0.0", "4.0.0"} {
		if err := m.VerifyVersionRoot(t.Context(), filepath.Join(m.root, VersionDir, version)); err != nil {
			t.Fatalf("受保护版本 %s 损坏：%v", version, err)
		}
	}
	if _, err := os.Stat(baseline); err != nil {
		t.Fatal("外部安装来源被删除")
	}
	if _, err := m.Rollback(t.Context(), m.VerifyVersionRoot); err != nil {
		t.Fatalf("清理后无法回滚：%v", err)
	}
	status, err := m.Status(t.Context())
	if err != nil || status.LastCleanup == nil || status.LastCleanup.RunningVersion != "4.0.0" {
		t.Fatalf("清理记录未持久化：%+v %v", status, err)
	}
}

func TestCleanupPreservesPendingPlansExternalPackagesAndUnknownFiles(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInstallation(t.Context(), makeBundle(t, "1.0.0", "安装基线"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	archiveFor := func(version string) string {
		t.Helper()
		root := makeBundle(t, version, version)
		manifest, err := os.ReadFile(filepath.Join(root, ManifestName))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "用户离线包.zip")
		if err := writeZIP(path, map[string][]byte{ManifestName: manifest, "bin/codex-chronicle": []byte(version)}); err != nil {
			t.Fatal(err)
		}
		return path
	}
	usedSource := archiveFor("2.0.0")
	used, err := m.PrepareArchive(t.Context(), usedSource, false)
	if err != nil {
		t.Fatal(err)
	}
	used.DownloadPath = filepath.Join(m.root, BundlesDirName, "download-"+strings.Repeat("a", 24)+".archive")
	if err := os.WriteFile(used.DownloadPath, []byte("已使用的下载包"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicJSON(t.Context(), m.planPath(used.ID), used); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), used, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	pending, err := m.PrepareArchive(t.Context(), archiveFor("3.0.0"), false)
	if err != nil {
		t.Fatal(err)
	}
	pending.DownloadPath = filepath.Join(m.root, BundlesDirName, "download-"+strings.Repeat("b", 24)+".archive")
	if err := os.WriteFile(pending.DownloadPath, []byte("待确认下载包"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicJSON(t.Context(), m.planPath(pending.ID), pending); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(m.root, BundlesDirName, "用户自己放入的文件.zip")
	if err := os.WriteFile(unknown, []byte("不属于更新缓存"), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownDir := filepath.Join(m.root, BundlesDirName, "download-"+strings.Repeat("c", 24)+".archive")
	if err := os.Mkdir(unknownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	retired := filepath.Join(m.root, VersionDir, ".cleanup-"+strings.Repeat("d", 24))
	if err := os.Mkdir(retired, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(retired, "上次清理的残余文件"), []byte("可安全重试"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmRunning(t.Context(), "2.0.0"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{used.DownloadPath, used.BundleRoot, retired} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("已用缓存未清理：%s", path)
		}
	}
	for _, path := range []string{usedSource, pending.DownloadPath, pending.BundleRoot, unknown, unknownDir} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("受保护文件被清理：%s %v", path, err)
		}
	}
	if _, err := m.ActivateByID(t.Context(), pending.ID, m.VerifyVersionRoot); err != nil {
		t.Fatalf("待确认计划无法继续执行：%v", err)
	}
}

func TestCleanupRefusesBusyOrInterruptedUpdates(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInstallation(t.Context(), makeBundle(t, "1.0.0", "基线"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmRunning(t.Context(), "1.0.0"); err == nil {
		t.Fatal("清理与进行中的下载未互斥")
	}
	lease.Close()
	if err := writeAtomicJSON(context.Background(), filepath.Join(m.root, JournalName), journal{Phase: "staging"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ConfirmRunning(t.Context(), "1.0.0"); err == nil {
		t.Fatal("中断更新未恢复就进行了清理")
	}
}
