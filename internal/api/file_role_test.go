// 中文说明：验证会话列表和普通/技术搜索共用文件角色边界；旧索引中的大量输入历史不能挤掉真实搜索结果。
package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/store"
)

func TestConversationAPIsExcludeSupportFilesEvenWithLegacySearchIndex(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"history.jsonl":                         strings.Repeat("{\"session_id\":\"real\",\"ts\":123,\"text\":\"roleboundaryneedle\"}\n", 501),
		".tmp/plugins/fixtures/responses.jsonl": "{\"type\":\"response.done\",\"response\":{\"usage\":{\"total_tokens\":42},\"metadata\":\"roleboundaryneedle\"}}\n",
		"z-real.jsonl":                          "{\"type\":\"session_meta\",\"payload\":{\"id\":\"real\"}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"roleboundaryneedle\"}}\n",
	}
	for relative, content := range files {
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	report, err := application.NewScanner().ScanWithoutGraphs(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟升级前已落盘的错误索引；测试中不调用新分类同步，要求查询本身也能拒绝旧错误结果。
	for i := range report.Files {
		file := &report.Files[i]
		if file.RelativePath == "z-real.jsonl" {
			continue
		}
		rollout, summary, err := compat.AnalyzeRolloutPath(filepath.Join(root, filepath.FromSlash(file.RelativePath)))
		if err != nil {
			t.Fatal(err)
		}
		file.Kind, file.Rollout, file.JSONL = domain.AssetKindJSONL, &rollout, &summary
	}
	if _, err := archive.SyncInventory(t.Context(), report); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.BuildSearchIndex(t.Context(), report, nil); err != nil {
		t.Fatal(err)
	}
	legacy, err := archive.Search(t.Context(), root, "roleboundaryneedle", 500)
	if err != nil || len(legacy) != 500 {
		t.Fatalf("未模拟出超出候选上限的旧索引污染: hits=%d err=%v", len(legacy), err)
	}
	server := NewServerWithStore(root, archive)
	defer server.Close()
	handler := server.Handler()
	response := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations", "")
	var list struct {
		Items []domain.ConversationSummary `json:"items"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Items) != 1 || list.Items[0].SourceID != "real" {
		t.Fatalf("会话列表仍显示辅助文件: %s", response.Body.String())
	}
	for _, scope := range []string{"effective", "technical"} {
		response := apiJSON(t, handler, http.MethodGet, "/api/v1/search?q=roleboundaryneedle&scope="+scope, "")
		var found struct {
			Hits []domain.SearchHit `json:"hits"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &found) != nil || len(found.Hits) != 1 || found.Hits[0].RelativePath != "z-real.jsonl" {
			t.Fatalf("辅助文件仍占用搜索名额: scope=%s response=%s", scope, response.Body.String())
		}
	}
	for _, relative := range []string{"history.jsonl", ".tmp/plugins/fixtures/responses.jsonl"} {
		id := compat.StableConversationID(filepath.Join(root, filepath.FromSlash(relative)))
		response := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations/"+id, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("旧辅助文件详情仍可作为会话访问: %s %s", relative, response.Body.String())
		}
	}
}
