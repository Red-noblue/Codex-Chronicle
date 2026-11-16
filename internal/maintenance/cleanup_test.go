// 中文说明：验证生成物登记、retention、未知对象报告、清理前后快照和中断恢复。
package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testCleanupRegistry(t *testing.T) (*Registry, string, string) {
	t.Helper()
	root := t.TempDir()
	registryRoot := filepath.Join(root, "registry")
	targetRoot := filepath.Join(root, "generated")
	if err := os.MkdirAll(targetRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(registryRoot)
	if err != nil {
		t.Fatal(err)
	}
	return registry, registryRoot, targetRoot
}

func deterministicCleanupProbe(context.Context, string) (bool, bool, string) {
	return false, true, "测试进程探测成功"
}

func TestQuarantineReportRejectsUnboundPathAndUnknownManifest(t *testing.T) {
	registry, registryRoot, targetRoot := testCleanupRegistry(t)
	target := filepath.Join(targetRoot, "sample")
	if err := os.WriteFile(target, []byte("sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, manifestPath, err := registry.RegisterArtifact(context.Background(), ArtifactManifest{Kind: ArtifactStaging, TargetPath: target, Owner: "test", Source: "test", Rebuildable: true})
	if err != nil {
		t.Fatal(err)
	}
	manifest.State, manifest.LastOperationID, manifest.QuarantinePath = "quarantined", "cleanup-operation", target
	candidate := registry.inspectManifest(context.Background(), manifestPath, manifest, CleanupOptions{}, time.Now())
	if candidate.Status != "unknown" || candidate.SizeBytes != 0 || candidate.Eligible {
		t.Fatalf("不应读取登记区之外的伪造隔离路径：%+v", candidate)
	}
	manifest.QuarantinePath = filepath.Join(registryRoot, cleanupQuarantineDir, manifest.LastOperationID, manifest.ArtifactID)
	manifest.Version = 999
	candidate = registry.inspectManifest(context.Background(), manifestPath, manifest, CleanupOptions{}, time.Now())
	if candidate.Status != "unknown" || candidate.Eligible {
		t.Fatalf("未知 manifest 版本不得作为隔离报告读取：%+v", candidate)
	}
}

func TestRegisterDiscoverAndRetentionExplainCandidate(t *testing.T) {
	registry, registryRoot, targetRoot := testCleanupRegistry(t)
	target := filepath.Join(targetRoot, "build-output")
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "nested", "result.bin"), []byte("rebuildable"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	manifest, manifestPath, err := registry.RegisterArtifact(context.Background(), ArtifactManifest{
		Kind: ArtifactRustTarget, TargetPath: target, Owner: "test", Source: "release build", Rebuildable: true,
		CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour), RetentionReason: "测试已完成",
	})
	if err != nil {
		t.Fatal(err)
	}
	if manifestPath == "" || manifest.ArtifactID == "" {
		t.Fatalf("登记结果缺少身份：%+v %s", manifest, manifestPath)
	}
	report, err := registry.Discover(context.Background(), CleanupOptions{Now: now, ProbeOpenFiles: true, OpenFileProbe: deterministicCleanupProbe})
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 1 || report.CandidateBytes == 0 || len(report.Unknown) != 0 {
		t.Fatalf("登记产物未成为可解释候选：%+v", report)
	}
	candidate := report.Candidates[0]
	if candidate.ArtifactID != manifest.ArtifactID || !candidate.Eligible || candidate.Status != "candidate" || candidate.FileCount != 1 || candidate.SizeBytes != int64(len("rebuildable")) {
		t.Fatalf("候选快照或状态错误：%+v", candidate)
	}
	if !strings.Contains(candidate.BlockReason, "已过期") {
		t.Fatalf("候选缺少可解释原因：%q", candidate.BlockReason)
	}
	if filepath.Dir(manifestPath) != filepath.Join(registryRoot, cleanupManifestDirectory) {
		t.Fatalf("manifest 未写入独立目录：%s", manifestPath)
	}
}

func TestDiscoverBlocksLeaseOpenProcessAndProtectedArtifacts(t *testing.T) {
	registry, _, targetRoot := testCleanupRegistry(t)
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	makeTarget := func(name string) string {
		path := filepath.Join(targetRoot, name)
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	leaseTarget := makeTarget("lease")
	openTarget := makeTarget("open")
	protectedTarget := makeTarget("protected")
	for _, manifest := range []ArtifactManifest{
		{Kind: ArtifactStaging, TargetPath: leaseTarget, Owner: "test", Source: "lease", Rebuildable: true, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour), LeaseUntil: now.Add(time.Hour)},
		{Kind: ArtifactStaging, TargetPath: openTarget, Owner: "test", Source: "open", Rebuildable: true, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		{Kind: ArtifactVerificationReport, TargetPath: protectedTarget, Owner: "test", Source: "formal result", Rebuildable: true, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
	} {
		if _, _, err := registry.RegisterArtifact(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(_ context.Context, path string) (bool, bool, string) {
		if path == openTarget {
			return true, true, "PID 42 打开目标"
		}
		return false, true, "测试进程探测成功"
	}
	report, err := registry.Discover(context.Background(), CleanupOptions{Now: now, ProbeOpenFiles: true, OpenFileProbe: probe})
	if err != nil {
		t.Fatal(err)
	}
	if report.CandidateCount != 0 || len(report.Candidates) != 0 {
		t.Fatalf("被阻断对象不应进入候选：%+v", report.Candidates)
	}
	if len(report.Retained) != 3 {
		t.Fatalf("阻断对象应保留在解释列表：%+v", report.Retained)
	}
	byPath := make(map[string]CleanupCandidate, len(report.Retained))
	for _, candidate := range report.Retained {
		byPath[candidate.TargetPath] = candidate
	}
	if !strings.Contains(byPath[leaseTarget].BlockReason, "租约") || !strings.Contains(byPath[openTarget].BlockReason, "打开") || !strings.Contains(byPath[protectedTarget].BlockReason, "正式验证") {
		t.Fatalf("阻断理由不完整：%+v", byPath)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("读取登记 manifest 出错：%v", report.Errors)
	}
}

func TestCleanupPlanExecuteAndRecoverPreservesJournal(t *testing.T) {
	registry, _, targetRoot := testCleanupRegistry(t)
	target := filepath.Join(targetRoot, "staging")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-24 * time.Hour)
	manifest, _, err := registry.RegisterArtifact(context.Background(), ArtifactManifest{Kind: ArtifactStaging, TargetPath: target, Owner: "test", Source: "interrupted task", Rebuildable: true, CreatedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	report, err := registry.Discover(context.Background(), CleanupOptions{Now: time.Now().UTC(), ProbeOpenFiles: true, OpenFileProbe: deterministicCleanupProbe})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := registry.BuildPlan(context.Background(), report, CleanupPlanOptions{SelectedIDs: []string{manifest.ArtifactID}, FixtureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.ExecutePlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("清理执行失败：%v", err)
	}
	if result.Phase != CleanupCommitted || len(result.Receipts) != 1 || result.JournalPath == "" {
		t.Fatalf("清理结果缺少审计信息：%+v", result)
	}
	if pathExists(target) || !pathExists(result.Receipts[0].QuarantinePath) {
		t.Fatalf("目标和隔离目录状态错误：target=%v quarantine=%v", pathExists(target), pathExists(result.Receipts[0].QuarantinePath))
	}
	if _, err := os.Stat(result.JournalPath); err != nil {
		t.Fatal(err)
	}
	interrupted, err := loadCleanupJournal(context.Background(), result.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	interrupted.Phase = CleanupApplied
	if err := writeJSONAtomic(result.JournalPath, interrupted, false); err != nil {
		t.Fatal(err)
	}
	recoveryReport, err := registry.Discover(context.Background(), CleanupOptions{Now: time.Now().UTC(), ProbeOpenFiles: true, OpenFileProbe: deterministicCleanupProbe})
	if err != nil || len(recoveryReport.Recoveries) != 1 || recoveryReport.Recoveries[0].OperationID != result.OperationID {
		t.Fatalf("中断清理 journal 未暴露给恢复 UI：report=%+v err=%v", recoveryReport, err)
	}
	recovered, err := registry.RecoverPlan(context.Background(), result.JournalPath)
	if err != nil || recovered.Phase != CleanupRolledBack {
		t.Fatalf("清理恢复失败：result=%+v err=%v", recovered, err)
	}
	if !pathExists(target) || pathExists(result.Receipts[0].QuarantinePath) {
		t.Fatalf("恢复后目标/隔离状态错误：target=%v quarantine=%v", pathExists(target), pathExists(result.Receipts[0].QuarantinePath))
	}
	restored, err := loadCleanupManifest(filepath.Join(registry.Root, cleanupManifestDirectory, manifest.ArtifactID+".json"))
	if err != nil || restored.State != "registered" || restored.QuarantinePath != "" {
		t.Fatalf("manifest 未恢复：%+v err=%v", restored, err)
	}
}

func TestCleanupReportsUnknownAndCrashResidueWithoutDeleting(t *testing.T) {
	registry, registryRoot, targetRoot := testCleanupRegistry(t)
	if err := os.MkdirAll(registryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(registryRoot, "unregistered-output")
	if err := os.WriteFile(unknown, []byte("unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	crash := filepath.Join(registryRoot, cleanupJournalDirectory, "unfinished.json.tmp")
	if err := os.MkdirAll(filepath.Dir(crash), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(crash, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetRoot, "expired.tmp")
	if err := os.WriteFile(target, []byte("expired"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.RegisterArtifact(context.Background(), ArtifactManifest{Kind: ArtifactTemporaryBinary, TargetPath: target, Owner: "test", Source: "failed build", Rebuildable: true, CreatedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	report, err := registry.Discover(context.Background(), CleanupOptions{ProbeOpenFiles: true, OpenFileProbe: deterministicCleanupProbe})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Unknown) == 0 || len(report.CrashResidues) == 0 || report.CandidateCount != 1 {
		t.Fatalf("未知对象或 crash residue 未报告：%+v", report)
	}
	if !pathExists(unknown) || !pathExists(crash) || !pathExists(target) {
		t.Fatal("只读发现意外修改了对象")
	}
}

func TestCleanupRejectsCodexTargetAndPersistsRetention(t *testing.T) {
	registry, _, targetRoot := testCleanupRegistry(t)
	if _, _, err := registry.RegisterArtifact(context.Background(), ArtifactManifest{Kind: ArtifactStaging, TargetPath: filepath.Join(targetRoot, ".codex", "bad"), Owner: "test", Source: "unsafe", Rebuildable: true}); err == nil || !strings.Contains(err.Error(), ".codex") {
		t.Fatalf("应拒绝 .codex 目标：%v", err)
	}
	policy := registry.Policy
	policy.StagingTTLSeconds = 123
	if err := registry.SavePolicy(context.Background(), policy); err != nil {
		t.Fatal(err)
	}
	reloaded, err := OpenRegistry(registry.Root)
	if err != nil || reloaded.Policy.StagingTTLSeconds != 123 {
		t.Fatalf("retention 未持久化：policy=%+v err=%v", reloaded.Policy, err)
	}
}
