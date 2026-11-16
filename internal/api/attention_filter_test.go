// 中文说明：验证待处理筛选覆盖活动会话和诊断会话，并与既有筛选组合。
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/domain"
)

func TestAttentionConversationFilter(t *testing.T) {
	items := []domain.ConversationSummary{
		{ID: "stable", Status: domain.StatusStable},
		{ID: "active", Status: domain.StatusActiveWriting},
		{ID: "diagnostic", Status: domain.StatusStable, Diagnostics: []domain.Diagnostic{{Code: "review"}}},
	}
	for _, test := range []struct {
		query string
		want  string
	}{
		{"attention=true", "active,diagnostic,"},
		{"attention=false", "stable,active,diagnostic,"},
		{"attention=true&status=stable", "diagnostic,"},
		{"attention=true&q=active", "active,"},
	} {
		t.Run(test.query, func(t *testing.T) {
			values, _ := url.ParseQuery(test.query)
			filter, err := parseConversationFilter(values, "")
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			for _, item := range items {
				if conversationMatchesFilter(item, filter, nil) {
					got += item.ID + ","
				}
			}
			if got != test.want {
				t.Fatalf("筛选结果 = %q，期望 %q", got, test.want)
			}
		})
	}
	if _, err := parseConversationFilter(url.Values{"attention": {"invalid"}}, ""); err == nil {
		t.Fatal("无效 attention 参数应被拒绝")
	}
}

func TestAttentionFilteringPrecedesPagination(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"first", "second", "third"} {
		content := `{"type":"session_meta","payload":{"id":"` + id + `"}}` + "\n"
		if id == "second" {
			content += "invalid-json\n"
		}
		if err := os.WriteFile(filepath.Join(root, "rollout-"+id+".jsonl"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handler := NewServer(root).Handler()
	readPage := func(query string) ([]domain.ConversationSummary, int) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations?"+query, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("读取列表失败: %d %s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Items []domain.ConversationSummary `json:"items"`
			Total int                          `json:"total"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Items, response.Total
	}
	all, _ := readPage("limit=100")
	want := 0
	for _, item := range all {
		if item.Status != domain.StatusStable || len(item.Diagnostics) > 0 {
			want++
		}
	}
	items, total := readPage("attention=true&limit=1")
	if want == 0 || total != want || len(items) != 1 {
		t.Fatalf("分页前筛选失效: total=%d want=%d items=%d", total, want, len(items))
	}
	for _, item := range items {
		if item.Status == domain.StatusStable && len(item.Diagnostics) == 0 {
			t.Fatalf("正常会话进入需关注队列: %s", item.ID)
		}
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/conversations?attention=invalid", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("无效筛选应返回 400，实际为 %d", recorder.Code)
	}
}
