// 中文说明：验证更新成功启动后的缓存淘汰、回滚与安装基线保留，以及待确认计划、下载租约和用户外部包的保护。
package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupWaitsForGracePeriodAndExpiresRollbackAndBaseline(t *testing.T) {
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
	now := time.Now().UTC()
	result, err := m.confirmRunningAt(t.Context(), "4.0.0", now)
	if err != nil || result.RemovedItems != 0 {
		t.Fatalf("观察期内提前清理：%+v %v", result, err)
	}
	status, err := m.Status(t.Context())
	if err != nil || status.State.CleanupAfter == nil || !status.State.CleanupAfter.Equal(now.Add(CleanupGracePeriod)) {
		t.Fatalf("未持久化清理时间：%+v %v", status, err)
	}
	// 模拟重启后重新建立管理器，观察期不能重新计时。
	m, err = NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.confirmRunningAt(t.Context(), "4.0.0", now.Add(CleanupGracePeriod-time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"1.0.0", "2.0.0", "3.0.0", "4.0.0"} {
		if err := m.VerifyVersionRoot(t.Context(), filepath.Join(m.root, VersionDir, version)); err != nil {
			t.Fatal(err)
		}
	}
	result, err = m.confirmRunningAt(t.Context(), "4.0.0", now.Add(CleanupGracePeriod))
	if err != nil || result.RemovedItems != 3 || result.FreedBytes == 0 {
		t.Fatalf("清理结果错误：%+v %v", result, err)
	}
	if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
		t.Fatal("未清理不再使用的历史版本")
	}
	for _, version := range []string{"4.0.0"} {
		if err := m.VerifyVersionRoot(t.Context(), filepath.Join(m.root, VersionDir, version)); err != nil {
			t.Fatalf("受保护版本 %s 损坏：%v", version, err)
		}
	}
	if _, err := os.Stat(baseline); err != nil {
		t.Fatal("外部安装来源被删除")
	}
	if _, err := m.Rollback(t.Context(), m.VerifyVersionRoot); err == nil {
		t.Fatal("过期后仍提供已清理的回滚版本")
	}
	status, err = m.Status(t.Context())
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
	now := time.Now().UTC()
	if _, err := m.confirmRunningAt(t.Context(), "2.0.0", now); err != nil {
		t.Fatal(err)
	}
	if _, err := m.confirmRunningAt(t.Context(), "2.0.0", now.Add(CleanupGracePeriod)); err != nil {
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

func TestCleanupRetentionReschedulesWithoutResettingHealthConfirmation(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterInstallation(t.Context(), makeBundle(t, "1.0.0", "首次安装"), "1.0.0"); err != nil {
		t.Fatal(err)
	}
	plan, err := m.Prepare(t.Context(), makeBundle(t, "2.0.0", "升级"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := m.confirmRunningAt(t.Context(), "2.0.0", now); err != nil {
		t.Fatal(err)
	}
	for _, minutes := range []int{5, 5, 30, 2} {
		if err := m.SetCleanupRetention(t.Context(), minutes); err != nil {
			t.Fatal(err)
		}
		m, err = NewManager(m.root)
		if err != nil {
			t.Fatal(err)
		}
		status, err := m.Status(t.Context())
		if err != nil || status.State.CleanupRetentionMinutes != minutes || status.State.ConfirmedAt == nil || !status.State.ConfirmedAt.Equal(now) || status.State.CleanupAfter == nil || !status.State.CleanupAfter.Equal(now.Add(time.Duration(minutes)*time.Minute)) {
			t.Fatalf("设置未持久化或重置了计时起点：%+v %v", status.State, err)
		}
		if err := m.VerifyVersionRoot(t.Context(), filepath.Join(m.root, VersionDir, "1.0.0")); err != nil {
			t.Fatal("保存设置时直接删除了旧版本", err)
		}
	}
	if result, err := m.confirmRunningAt(t.Context(), "2.0.0", now.Add(2*time.Minute-time.Second)); err != nil || result.RemovedItems != 0 {
		t.Fatalf("缩短后的期限到达前提前清理：%+v %v", result, err)
	}
	if result, err := m.confirmRunningAt(t.Context(), "2.0.0", now.Add(2*time.Minute)); err != nil || result.RemovedItems != 1 {
		t.Fatalf("缩短后的期限没有执行清理：%+v %v", result, err)
	}
	if err := m.SetCleanupRetention(t.Context(), 7); err != nil {
		t.Fatal(err)
	}
	status, err := m.Status(t.Context())
	if err != nil || status.State.CleanupAfter != nil || status.State.PreviousVersion != "" {
		t.Fatal("延长时长重新启用了已清理的回退窗口", err)
	}
	plan, err = m.Prepare(t.Context(), makeBundle(t, "3.0.0", "再次升级"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := m.confirmRunningAt(t.Context(), "3.0.0", now); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status(t.Context())
	if err != nil || status.State.CleanupRetentionMinutes != 7 || status.State.CleanupAfter == nil || !status.State.CleanupAfter.Equal(now.Add(7*time.Minute)) {
		t.Fatal("后续更新没有沿用已保存的保留时长", err)
	}
	if _, err := m.Rollback(t.Context(), m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	status, err = m.Status(t.Context())
	if err != nil || status.State.CleanupRetentionMinutes != 7 {
		t.Fatal("回滚丢失了清理设置", err)
	}
}

func TestCleanupRetentionRejectsInvalidValuesAndBusyOrInterruptedUpdates(t *testing.T) {
	m, err := NewManager(filepath.Join(t.TempDir(), "updates"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetCleanupRetention(t.Context(), 1440); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(m.root, StateName))
	if err != nil {
		t.Fatal(err)
	}
	for _, minutes := range []int{-1, 0, 43201} {
		if err := m.SetCleanupRetention(t.Context(), minutes); err == nil {
			t.Fatalf("接受了非法保留时长 %d", minutes)
		}
	}
	lease, err := acquireUpdateLock(filepath.Join(m.root, ".download.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetCleanupRetention(t.Context(), 1); err == nil {
		t.Fatal("更新占用期间允许修改清理期限")
	}
	lease.Close()
	if err := writeAtomicJSON(t.Context(), filepath.Join(m.root, JournalName), journal{Phase: "staging"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetCleanupRetention(t.Context(), 1); err == nil {
		t.Fatal("更新恢复完成前允许修改清理期限")
	}
	after, err := os.ReadFile(filepath.Join(m.root, StateName))
	if err != nil || string(before) != string(after) {
		t.Fatal("被拒绝的设置修改了安装状态", err)
	}
}
