// 中文说明：验证目录选择覆盖数千个未加载候选、精确限制路径、排除数据库等保留项，且不会隐式修复会话。
package maintenance

import (
	"fmt"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestDirectorySelectionCoversAllPagesAndReportsRetainedFiles(t *testing.T) {
	report := domain.InventoryReport{Root: "/fixture", ReadOnly: true}
	for i := 0; i < 5500; i++ {
		report.Files = append(report.Files, domain.FileInventory{Asset: domain.Asset{RelativePath: fmt.Sprintf(".tmp/plugin/data-%05d.bin", i), SizeBytes: 2, Status: domain.StatusStable}})
	}
	for _, relative := range []string{".tmp/state.sqlite", ".tmp/state.sqlite-wal", ".tmp/state.sqlite-shm", ".tmp/auth.json", ".tmp/credentials.json", ".tmp/config.toml", ".tmp/plugins.sync.lock", ".tmp/rollout-fixture.jsonl", ".tmp/history.jsonl", ".tmp/session_index.jsonl"} {
		report.Files = append(report.Files, domain.FileInventory{Asset: domain.Asset{RelativePath: relative, SizeBytes: 3, Status: domain.StatusStable}})
	}
	report.Files = append(report.Files,
		domain.FileInventory{Asset: domain.Asset{RelativePath: ".tmp/active.bin", SizeBytes: 3, Status: domain.StatusActiveWriting}},
		domain.FileInventory{Asset: domain.Asset{RelativePath: ".tmp/pending.jsonl", SizeBytes: 3, Status: domain.StatusStable}, JSONL: &domain.JSONLScanSummary{PendingLines: 1, InvalidJSONLines: 1}},
		domain.FileInventory{Asset: domain.Asset{RelativePath: ".tmp-other/keep.bin", SizeBytes: 50, Status: domain.StatusStable}},
		domain.FileInventory{Asset: domain.Asset{RelativePath: "sessions/rollout-real.jsonl", SizeBytes: 100, Status: domain.StatusStable}},
	)
	page := BuildReportWithOptions(report, ReportOptions{Limit: 30})
	if len(page.Candidates) != 30 || len(page.Directories) != 1 {
		t.Fatalf("列表分页不应截断目录汇总：candidates=%d directories=%+v", len(page.Candidates), page.Directories)
	}
	directory := page.Directories[0]
	if directory.EligibleCount != 5500 || directory.RetainedCount != 12 || directory.EligibleBytes != 11000 || directory.FileCount != 5512 {
		t.Fatalf("目录汇总不准确：%+v", directory)
	}
	selection, err := ResolveSelection(report, []string{".tmp/plugin/data-00001.bin"}, []string{".tmp", ".tmp"})
	if err != nil || selection.FileCount != 5500 || len(selection.Paths) != 5500 || selection.SizeBytes != 11000 || selection.RetainedCount != 12 || len(selection.Directories) != 1 {
		t.Fatalf("目录选择应去重并涵盖全部分页：selection=%+v err=%v", selection, err)
	}
	for _, relative := range selection.Paths {
		if relative < ".tmp/plugin/data-00000.bin" || relative > ".tmp/plugin/data-05499.bin" {
			t.Fatalf("受保护或非目标文件进入清理范围：%s", relative)
		}
	}
	filtered := BuildReportWithOptions(report, ReportOptions{Directory: ".tmp", Query: "data-05499", Limit: 1})
	if filtered.FilteredCount != 1 || filtered.Candidates[0].RelativePath != ".tmp/plugin/data-05499.bin" || filtered.Directories[0].EligibleCount != 5500 {
		t.Fatalf("目录内筛选应先于分页，且不缩小目录选择范围：%+v", filtered)
	}
}

func TestDirectorySelectionRejectsArbitraryAndEscapingDirectories(t *testing.T) {
	report := domain.InventoryReport{Files: []domain.FileInventory{{Asset: domain.Asset{RelativePath: "tmp/item.bin", Status: domain.StatusStable}}}}
	for _, directory := range []string{"", ".", "..", "../tmp", "/tmp", "tmp/", "tmp/../tmp", `tmp\other`, "sessions", "tmp/item.bin", "cache", "tmp\x00"} {
		if _, err := ResolveSelection(report, nil, []string{directory}); err == nil {
			t.Errorf("不应允许目录 %q", directory)
		}
	}
}

func TestDirectorySelectionIncludesPluginCacheAndKeepsDatabaseAuxiliaryBlocked(t *testing.T) {
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "plugins/cache/package/item.bin", SizeBytes: 5, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "plugins/cache/package/state.sqlite-wal", SizeBytes: 7, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "plugins/cache-other/keep.bin", Status: domain.StatusStable}},
	}}
	selection, err := ResolveSelection(report, nil, []string{"plugins/cache"})
	if err != nil || selection.FileCount != 1 || selection.Paths[0] != "plugins/cache/package/item.bin" || selection.RetainedCount != 1 || selection.SizeBytes != 5 {
		t.Fatalf("插件缓存应按完整目录选择，不能包含同名前缀或 WAL：%+v err=%v", selection, err)
	}
	candidate, ok := candidateForFile(report.Files[1])
	if !ok || candidate.Eligible || candidate.Category != "sqlite_auxiliary" {
		t.Fatalf("缓存路径中的 WAL 也必须保持保护：%+v", candidate)
	}
}

func TestUngroupedScopeFiltersBeforePagingAndExplainsProtectedFiles(t *testing.T) {
	report := domain.InventoryReport{Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: ".tmp/large.bin", SizeBytes: 999, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite-wal", SizeBytes: 8, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "memories/.git/logs/HEAD", SizeBytes: 7, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "daemon/stderr.log", SizeBytes: 6, Status: domain.StatusStable}},
		// 不属于维护候选的文件不能影响独立候选计数。
		{Asset: domain.Asset{RelativePath: ".tmp/unknown.bin", SizeBytes: 5, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "sessions/untouched.jsonl", SizeBytes: 4, Status: domain.StatusStable}},
	}}
	page := BuildReportWithOptions(report, ReportOptions{Scope: "ungrouped", Limit: 1})
	if page.UngroupedCount != 3 || page.UngroupedBlockedCount != 2 || page.FilteredCount != 3 || len(page.Candidates) != 1 || page.Candidates[0].RelativePath != "state.sqlite-wal" || !page.HasMore {
		t.Fatalf("独立文件范围必须先筛选后分页：%+v", page)
	}
	blocked := BuildReportWithOptions(report, ReportOptions{Scope: "ungrouped", Eligibility: "blocked"})
	if blocked.FilteredCount != 2 || blocked.Candidates[1].RelativePath != "memories/.git/logs/HEAD" || blocked.Candidates[1].BlockReason == "" {
		t.Fatalf("受保护 Git 日志必须给出保留原因：%+v", blocked)
	}
	eligible := BuildReportWithOptions(report, ReportOptions{Scope: "ungrouped", Eligibility: "eligible"})
	if len(eligible.Candidates) != 1 || eligible.Candidates[0].RelativePath != "daemon/stderr.log" {
		t.Fatalf("运行日志不能与 Git 内部日志混淆：%+v", eligible)
	}
}
