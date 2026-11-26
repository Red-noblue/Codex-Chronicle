// 中文说明：模拟旧版错误索引输入历史，验证类型纠正后仅清除本项目派生索引，保留源文件和正常会话搜索。
package store

import (
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
)

func TestSyncReclassifiedJSONLRemovesLegacyConversationProjection(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	files := map[string]string{
		"history.jsonl":      "{\"session_id\":\"target\",\"ts\":123,\"text\":\"classificationneedle\"}\n",
		"rollout-real.jsonl": "{\"type\":\"session_meta\",\"payload\":{\"id\":\"real\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"classificationneedle\"}}\n",
	}
	for relative, content := range files {
		if err := os.WriteFile(filepath.Join(root, relative), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	enableSearchIndex(t, archive)
	old, err := application.NewScanner().ScanWithoutGraphs(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	for i := range old.Files {
		file := &old.Files[i]
		if file.RelativePath != "history.jsonl" {
			continue
		}
		projection, summary, err := compat.AnalyzeRolloutPath(filepath.Join(root, file.RelativePath))
		if err != nil {
			t.Fatal(err)
		}
		file.Kind, file.Rollout, file.JSONL = domain.AssetKindJSONL, &projection, &summary
		old.Conversations = append(old.Conversations, domain.ConversationSummary{ID: projection.ConversationID, SourceID: projection.SourceID, Status: projection.Status, RolloutPaths: []string{file.RelativePath}})
	}
	if _, err := archive.SyncInventory(ctx, old); err != nil {
		t.Fatal(err)
	}
	legacy, err := archive.Search(ctx, root, "classificationneedle", 100)
	if err != nil || len(legacy) != 2 {
		t.Fatalf("未成功模拟旧索引: hits=%+v err=%v", legacy, err)
	}
	current, err := application.NewScanner().ScanWithoutGraphs(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := archive.SyncInventory(ctx, current); err != nil {
		t.Fatal(err)
	}
	hits, err := archive.Search(ctx, root, "classificationneedle", 100)
	if err != nil || len(hits) != 1 || hits[0].RelativePath != "rollout-real.jsonl" {
		t.Fatalf("分类纠正后仍有幽灵搜索结果: hits=%+v err=%v", hits, err)
	}
	for _, table := range []string{"jsonl_frames", "jsonl_cursors", "search_locations"} {
		var count int
		if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+" WHERE asset_id IN (SELECT id FROM assets WHERE relative_path='history.jsonl')").Scan(&count); err != nil || count != 0 {
			t.Fatalf("旧投影未清理: table=%s count=%d err=%v", table, count, err)
		}
	}
	var count int
	if err := archive.db.QueryRowContext(ctx, "SELECT count(*) FROM conversations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("旧伪会话仍保留: %d %v", count, err)
	}
	for relative, expected := range files {
		actual, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || string(actual) != expected {
			t.Fatalf("索引同步改写了源文件: %s %v", relative, err)
		}
	}
}
