// 中文说明：验证维护候选分类、JSONL 末行修复、回收站回滚和真实 .codex 写入门禁。
package maintenance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/catalog"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/platform"
)

func TestBuildReportSeparatesRepairAndBlockedCandidates(t *testing.T) {
	root := t.TempDir()
	tailPath := filepath.Join(root, "sessions", "tail.jsonl")
	if err := os.MkdirAll(filepath.Dir(tailPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tailPath, []byte(`{"type":"ok"}`+"\n"+`{"type":"broken"`), 0o600); err != nil {
		t.Fatal(err)
	}
	tailInfo, err := os.Stat(tailPath)
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(root, "tmp", "stale.tmp")
	if err := os.MkdirAll(filepath.Dir(tmpPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmpPath, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpInfo, err := os.Stat(tmpPath)
	if err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(root, "state.sqlite-wal")
	if err := os.WriteFile(walPath, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	walInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatal(err)
	}
	report := domain.InventoryReport{Root: root, ReadOnly: true, Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "sessions/tail.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: tailInfo.Size(), ModTime: tailInfo.ModTime().UTC(), Status: domain.StatusStable}, JSONL: &domain.JSONLScanSummary{PendingLines: 1, InvalidJSONLines: 1}},
		{Asset: domain.Asset{RelativePath: "tmp/stale.tmp", Kind: domain.AssetKindUnknown, SizeBytes: tmpInfo.Size(), ModTime: tmpInfo.ModTime().UTC(), Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite-wal", Kind: domain.AssetKindUnknown, SizeBytes: walInfo.Size(), ModTime: walInfo.ModTime().UTC(), Status: domain.StatusStable}},
	}}
	result := BuildReport(report)
	if result.CandidateCount != 3 || result.EligibleCount != 2 {
		t.Fatalf("unexpected maintenance report: %+v", result)
	}
	byPath := make(map[string]Candidate, len(result.Candidates))
	for _, candidate := range result.Candidates {
		byPath[candidate.RelativePath] = candidate
	}
	if byPath["sessions/tail.jsonl"].Action != ActionRepairJSONLTail || !byPath["sessions/tail.jsonl"].Eligible {
		t.Fatalf("tail repair candidate was not eligible: %+v", byPath["sessions/tail.jsonl"])
	}
	if byPath["state.sqlite-wal"].Eligible || byPath["state.sqlite-wal"].BlockReason == "" {
		t.Fatalf("WAL candidate was not blocked: %+v", byPath["state.sqlite-wal"])
	}
}

func TestBuildReportPageKeepsTotalsAndStableOrdering(t *testing.T) {
	report := domain.InventoryReport{GeneratedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), ReadOnly: true, Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "tmp/small.tmp", SizeBytes: 2, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "tmp/large.tmp", SizeBytes: 8, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "tmp/middle.tmp", SizeBytes: 5, Status: domain.StatusUnknown}},
	}}
	page := BuildReportPage(report, 1, 1)
	if page.CandidateCount != 3 || page.CandidateBytes != 15 || page.EligibleCount != 2 {
		t.Fatalf("page lost report totals: %+v", page)
	}
	if page.CandidateOffset != 1 || page.CandidateLimit != 1 || !page.HasMore || len(page.Candidates) != 1 || page.Candidates[0].RelativePath != "tmp/middle.tmp" {
		t.Fatalf("unexpected maintenance page: %+v", page)
	}
	last := BuildReportPage(report, 2, 1)
	if last.HasMore || len(last.Candidates) != 1 || last.Candidates[0].RelativePath != "tmp/small.tmp" {
		t.Fatalf("unexpected final maintenance page: %+v", last)
	}
}

func TestMaintenanceFiltersBeforePaginationAndPreservesSourceTotals(t *testing.T) {
	now := time.Now().UTC()
	report := domain.InventoryReport{Root: "/fixture", Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "tmp/A.tmp", SizeBytes: 8, Status: domain.StatusStable, ModTime: now}},
		{Asset: domain.Asset{RelativePath: "tmp/B.tmp", SizeBytes: 3, Status: domain.StatusStable, ModTime: now.Add(-time.Hour)}},
		{Asset: domain.Asset{RelativePath: "tmp/C.tmp", SizeBytes: 6, Status: domain.StatusActiveWriting, ModTime: now.Add(-2 * time.Hour)}},
		{Asset: domain.Asset{RelativePath: "cache/插件/data.bin", SizeBytes: 10, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite-wal", SizeBytes: 4, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "config.toml", SizeBytes: 100, Status: domain.StatusStable}},
	}}
	page := BuildReportWithOptions(report, ReportOptions{Category: "temporary", Eligibility: "eligible", Sort: "oldest", Offset: 1, Limit: 1})
	if page.SourceFileCount != 6 || page.SourceBytes != 131 || page.CandidateCount != 5 || page.CandidateBytes != 31 || page.EligibleCount != 3 || page.BlockedCount != 2 {
		t.Fatalf("筛选不应影响来源和完整候选统计：%+v", page)
	}
	if page.FilteredCount != 2 || page.FilteredBytes != 11 || len(page.Candidates) != 1 || page.Candidates[0].RelativePath != "tmp/A.tmp" || page.HasMore {
		t.Fatalf("应先筛选排序再分页：%+v", page)
	}
	if len(page.Categories) != 3 {
		t.Fatalf("分类汇总应覆盖完整快照：%+v", page.Categories)
	}
	for _, test := range []struct{ query, eligibility, path string }{
		{"TMP/b", "", "tmp/B.tmp"}, {"插件", "", "cache/插件/data.bin"}, {"", "blocked", "tmp/C.tmp"},
	} {
		result := BuildReportWithOptions(report, ReportOptions{Query: test.query, Eligibility: test.eligibility, Limit: 1})
		if len(result.Candidates) != 1 || result.Candidates[0].RelativePath != test.path {
			t.Fatalf("检索范围错误：%+v", result)
		}
	}
	empty := BuildReportWithOptions(report, ReportOptions{Query: "不存在"})
	if empty.FilteredCount != 0 || empty.FilteredBytes != 0 || empty.CandidateCount != 5 || len(empty.Candidates) != 0 {
		t.Fatalf("空结果丢失完整统计：%+v", empty)
	}
}

func TestExecuteRepairsJSONLTailWithBackupAndManifest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	original := []byte(`{"type":"session_meta"}` + "\n" + `{"type":"incomplete"`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	report := domain.InventoryReport{Root: root, ReadOnly: true, Files: []domain.FileInventory{{Asset: domain.Asset{RelativePath: "rollout.jsonl", Kind: domain.AssetKindJSONL, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}, JSONL: &domain.JSONLScanSummary{PendingLines: 1, InvalidJSONLines: 1}}}}
	plan, err := BuildPlan(context.Background(), root, report, PlanOptions{SelectedPaths: []string{"rollout.jsonl"}, FixtureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	journalRoot := filepath.Join(t.TempDir(), "journal")
	registry, _, _ := testCleanupRegistry(t)
	registered := make([]ArtifactManifest, 0, 2)
	registerArtifact := func(ctx context.Context, manifest ArtifactManifest) (ArtifactManifest, error) {
		stored, _, registerErr := registry.RegisterArtifact(ctx, manifest)
		if registerErr == nil {
			registered = append(registered, stored)
		}
		return stored, registerErr
	}
	result, err := Execute(context.Background(), plan, ExecuteOptions{JournalRoot: journalRoot, RegisterArtifact: registerArtifact})
	if err != nil {
		t.Fatalf("repair failed: %v", err)
	}
	if result.Phase != PhaseCommitted || result.ManifestPath == "" || result.JournalPath == "" || len(result.Backups) != 1 {
		t.Fatalf("unexpected repair result: %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"session_meta"}`+"\n" {
		t.Fatalf("unexpected repaired content: %q", data)
	}
	summary, err := catalog.ScanReader(strings.NewReader(string(data)), nil)
	if err != nil || summary.InvalidJSONLines != 0 || summary.PendingLines != 0 {
		t.Fatalf("repaired JSONL is not valid: summary=%+v err=%v", summary, err)
	}
	if _, err := os.Stat(result.Backups[0].BackupPath); err != nil {
		t.Fatalf("repair backup missing: %v", err)
	}
	if len(registered) != 2 || registered[len(registered)-1].State != "published" || registered[len(registered)-1].TargetPath != filepath.Join(journalRoot, plan.OperationID) {
		t.Fatalf("maintenance operation root was not registered through its lifecycle: %+v", registered)
	}
}

func TestExecuteIsolateRollsBackWhenSecondMoveFails(t *testing.T) {
	root := t.TempDir()
	paths := []string{"a.tmp", "b.tmp"}
	files := make([]domain.FileInventory, 0, len(paths))
	for _, relative := range paths {
		path := filepath.Join(root, relative)
		if err := os.WriteFile(path, []byte(relative), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, domain.FileInventory{Asset: domain.Asset{RelativePath: relative, Kind: domain.AssetKindUnknown, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}})
	}
	plan, err := BuildPlan(context.Background(), root, domain.InventoryReport{Root: root, ReadOnly: true, Files: files}, PlanOptions{SelectedPaths: paths, FixtureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	trashRoot := filepath.Join(t.TempDir(), "trash")
	trash, err := platform.NewDirectoryTrash(trashRoot)
	if err != nil {
		t.Fatal(err)
	}
	failing := &failingTrash{delegate: trash, failAfter: 1}
	result, err := Execute(context.Background(), plan, ExecuteOptions{JournalRoot: filepath.Join(t.TempDir(), "journal"), Trash: failing})
	if err == nil || result.Phase != PhaseRolledBack || result.NeedsReview {
		t.Fatalf("expected clean rollback, result=%+v err=%v", result, err)
	}
	for _, relative := range paths {
		if _, statErr := os.Stat(filepath.Join(root, relative)); statErr != nil {
			t.Fatalf("source was not restored: %s: %v", relative, statErr)
		}
	}
}

func TestExecuteRejectsCodexRootEvenForFixturePlan(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".codex", "copy")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "stale.tmp")
	if err := os.WriteFile(path, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), root, domain.InventoryReport{Root: root, ReadOnly: true, Files: []domain.FileInventory{{Asset: domain.Asset{RelativePath: "stale.tmp", SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}}}}, PlanOptions{SelectedPaths: []string{"stale.tmp"}, FixtureOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Execute(context.Background(), plan, ExecuteOptions{JournalRoot: filepath.Join(base, "journal"), TrashRoot: filepath.Join(base, "trash")}); err == nil || !strings.Contains(err.Error(), ".codex") {
		t.Fatalf("expected .codex refusal, err=%v", err)
	}
}

func TestRecoverRestoresInterruptedJSONLRepair(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	original := []byte(`{"type":"session_meta"}` + "\n" + `{"type":"incomplete"`)
	repaired := []byte(`{"type":"session_meta"}` + "\n")
	if err := os.WriteFile(path, repaired, 0o600); err != nil {
		t.Fatal(err)
	}
	backupPath := filepath.Join(t.TempDir(), "backup.jsonl")
	if err := os.WriteFile(backupPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	originalHash := sha256Hex(original)
	repairedHash := sha256Hex(repaired)
	plan := Plan{Version: 1, OperationID: "maintenance-recovery-test", Root: root, Status: PlanPrepared, WriteEnabled: true, Assets: []PlanAsset{{RelativePath: "rollout.jsonl", Action: ActionRepairJSONLTail, SizeBytes: int64(len(original)), SHA256: originalHash}}}
	journalPath := filepath.Join(t.TempDir(), "maintenance-recovery-test.journal.json")
	entry := journal{Version: 1, OperationID: plan.OperationID, Phase: PhaseApplied, Plan: plan, Backups: []BackupRecord{{RelativePath: "rollout.jsonl", BackupPath: backupPath, OriginalSHA256: originalHash, RepairedSHA256: repairedHash, OriginalSizeBytes: int64(len(original))}}}
	if err := writeJSONAtomic(journalPath, entry, true); err != nil {
		t.Fatal(err)
	}
	result, err := Recover(context.Background(), journalPath, nil)
	if err != nil || result.Phase != PhaseRolledBack {
		t.Fatalf("recovery failed: result=%+v err=%v", result, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(original) {
		t.Fatalf("recovery did not restore original bytes: %q", data)
	}
}

type failingTrash struct {
	delegate  platform.TrashProvider
	failAfter int
	moves     int
}

func sha256Hex(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func (f *failingTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (platform.TrashReceipt, error) {
	f.moves++
	if f.moves > f.failAfter {
		return platform.TrashReceipt{}, errors.New("注入回收站失败")
	}
	return f.delegate.Move(ctx, operationID, sourceRoot, relativePath)
}

func (f *failingTrash) Restore(ctx context.Context, receipt platform.TrashReceipt, sourceRoot string) error {
	return f.delegate.Restore(ctx, receipt, sourceRoot)
}
