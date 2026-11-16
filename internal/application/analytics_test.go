// 中文说明：验证统计趋势和维护候选只读投影，不执行任何清理动作。
package application

import (
	"testing"
	"time"

	"codex-chronicle.local/internal/domain"
)

func TestBuildAnalyticsAggregatesTokensAndDailyUsage(t *testing.T) {
	report := domain.InventoryReport{
		ReadOnly: true, GeneratedAt: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		Files: []domain.FileInventory{{Asset: domain.Asset{RelativePath: "sessions/one.jsonl", SizeBytes: 40}}},
		Conversations: []domain.ConversationSummary{
			{ID: "one", CWD: "/work/a", Model: "gpt", UpdatedAt: time.Date(2026, 10, 28, 1, 0, 0, 0, time.UTC), UserMessages: 2, AssistantMessages: 1, TurnCount: 2, TokenUsage: domain.TokenUsage{TotalTokens: 10}, Status: domain.StatusStable, RolloutPaths: []string{"sessions/one.jsonl"}},
			{ID: "two", CWD: "/work/b", Model: "gpt", UpdatedAt: time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC), UserMessages: 1, AssistantMessages: 1, TurnCount: 1, TokenUsage: domain.TokenUsage{TotalTokens: 20}, Status: domain.StatusConflict},
		},
	}
	result := BuildAnalytics(report)
	if !result.ReadOnly || result.FileCount != 1 || result.TotalBytes != 40 || result.ConversationCount != 2 {
		t.Fatalf("unexpected overview: %+v", result)
	}
	if result.StableCount != 1 || result.BlockedCount != 1 || result.TokenUsage.TotalTokens != 30 || result.MessageCount != 5 || result.TurnCount != 3 {
		t.Fatalf("unexpected totals: %+v", result)
	}
	if len(result.Daily) != 1 || result.Daily[0].Date != "2026-10-28" || result.Daily[0].TotalTokens != 30 {
		t.Fatalf("unexpected daily trend: %+v", result.Daily)
	}
	if len(result.Models) != 1 || result.Models[0].Name != "gpt" || result.Models[0].Count != 2 {
		t.Fatalf("unexpected model distribution: %+v", result.Models)
	}
}

func TestBuildMaintenanceReportClassifiesCandidatesWithoutDeleting(t *testing.T) {
	report := domain.InventoryReport{ReadOnly: true, Files: []domain.FileInventory{
		{Asset: domain.Asset{RelativePath: "tmp/one.tmp", SizeBytes: 10, Status: domain.StatusStable}},
		{Asset: domain.Asset{RelativePath: "state.sqlite-wal", SizeBytes: 20, Status: domain.StatusActiveWriting}},
		{Asset: domain.Asset{RelativePath: "sessions/one.jsonl", SizeBytes: 30, Status: domain.StatusStable}},
	}}
	result := BuildMaintenanceReport(report)
	if !result.ReadOnly || result.CandidateCount != 2 || result.CandidateBytes != 30 {
		t.Fatalf("unexpected maintenance report: %+v", result)
	}
	if result.Candidates[0].Category != "sqlite_auxiliary" || result.Candidates[0].Risk != "high" || result.Candidates[0].Status != domain.StatusActiveWriting {
		t.Fatalf("unexpected candidate ordering or risk: %+v", result.Candidates)
	}
	if !result.Candidates[0].Recoverable || !result.Candidates[1].Recoverable {
		t.Fatal("maintenance candidates must remain recoverable/read-only")
	}
}

func TestBuildAnalyticsSharedBytesAndUnknownDates(t *testing.T) {
	report := domain.InventoryReport{
		ReadOnly: true,
		Files: []domain.FileInventory{
			{Asset: domain.Asset{RelativePath: "shared", SizeBytes: 100}},
			{Asset: domain.Asset{RelativePath: "own", SizeBytes: 40}},
		},
		Conversations: []domain.ConversationSummary{
			{CWD: " /work/a/./ ", Model: " gpt ", RolloutPaths: []string{"own", "own"}, RelatedAssetPaths: []string{"shared"}, UpdatedAt: time.Date(2026, 10, 2, 0, 30, 0, 0, time.FixedZone("UTC+8", 8*3600))},
			{CWD: "/work/a", Model: "gpt", RelatedAssetPaths: []string{"shared"}, StartedAt: time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
			{CWD: "/work/b", RelatedAssetPaths: []string{"shared"}},
			{},
		},
	}
	result := BuildAnalytics(report)
	projects := map[string]NamedCount{}
	for _, item := range result.Projects {
		projects[item.Name] = item
	}
	if projects["/work/a"].TotalBytes != 140 || projects["/work/a"].Count != 2 || projects["/work/b"].TotalBytes != 100 || result.TotalBytes != 140 {
		t.Fatalf("共享文件应在同项目内去重，跨项目保留归属: %+v", result)
	}
	if result.ConversationCount != 4 || len(result.Daily) != 2 || result.Daily[0].Date != "2026-10-01" || result.Daily[1].Date != "2026-10-03" {
		t.Fatalf("日期应使用 UTC 和开始时间回退，未知日期不能伪造: %+v", result.Daily)
	}
	if projects["未知项目"].Count != 1 || len(result.Models) != 2 {
		t.Fatalf("未知项和 trim 分组应保留: %+v", result)
	}
}
