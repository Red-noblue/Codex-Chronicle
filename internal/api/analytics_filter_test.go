// 中文说明：验证统计筛选与会话下钻口径一致、精确分组不混入前缀项，且查询不改变源文件或完整快照。
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/domain"
)

func TestAnalyticsExactFiltersMatchConversationDrilldown(t *testing.T) {
	items := []domain.ConversationSummary{
		{ID: "parent", CWD: "/work/a", Model: "gpt", ModelProvider: "provider"},
		{ID: "child", CWD: "/work/a/child", Model: "gpt-plus"},
		{ID: "normalized", CWD: " /work/a/./ ", Model: " gpt "},
		{ID: "unknown"},
	}
	for _, test := range []struct{ query, want string }{
		{"model=gpt", "parent,child,normalized,"},
		{"model=gpt&model_exact=true", "parent,normalized,"},
		{"model=provider&model_exact=true", ""},
		{"model=provider", "parent,"},
		{"cwd=/work/a", "parent,child,normalized,"},
		{"cwd=/work/a&cwd_exact=true", "parent,normalized,"},
		{"model=未知模型&model_exact=true&cwd=未知项目&cwd_exact=true", "unknown,"},
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
				t.Fatalf("结果 %q，期望 %q", got, test.want)
			}
		})
	}
	if _, err := parseConversationFilter(url.Values{"model_exact": {"invalid"}}, ""); err == nil {
		t.Fatal("无效 exact 参数应被拒绝")
	}
}

func TestAnalyticsScopedAPIIsReadOnlyAndKeepsFullSnapshot(t *testing.T) {
	root := t.TempDir()
	original := map[string][]byte{}
	for index, item := range []struct{ model, cwd, date string }{
		{"gpt", "/work/a", "2026-10-01T23:59:59Z"},
		{"gpt-plus", "/work/a/child", "2026-10-02T00:00:00Z"},
		{"gpt", "/work/b", "2026-10-03T00:00:00Z"},
		{"", "", ""},
	} {
		content := fmt.Sprintf("{\"type\":\"session_meta\",\"timestamp\":%q,\"payload\":{\"id\":%q,\"model\":%q,\"cwd\":%q}}\n", item.date, fmt.Sprint(index), item.model, item.cwd)
		path := filepath.Join(root, fmt.Sprintf("rollout-%d.jsonl", index))
		original[path] = []byte(content)
		if err := os.WriteFile(path, original[path], 0o600); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(root, "unused.tmp")
	original[unrelated] = []byte("not-associated")
	if err := os.WriteFile(unrelated, original[unrelated], 0o600); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	read := func(path string, target any) {
		t.Helper()
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
			t.Fatal(err)
		}
	}
	type analyticsResponse struct {
		application.AnalyticsReport
		Filtered        bool `json:"filtered"`
		SourceFileCount int  `json:"source_file_count"`
		Undated         int  `json:"undated_conversation_count"`
	}
	for _, test := range []struct {
		query string
		want  int
	}{
		{"from=2026-10-01&to=2026-10-01", 1},
		{"from=2026-10-01&to=2026-10-03&model=gpt&model_exact=true&cwd=/work/a&cwd_exact=true", 1},
		{"model=gpt&model_exact=true", 2},
		{"model=未知模型&model_exact=true&cwd=未知项目&cwd_exact=true", 1},
		{"model=absent&model_exact=true", 0},
	} {
		t.Run(test.query, func(t *testing.T) {
			var analytics analyticsResponse
			var conversations struct {
				Items []domain.ConversationSummary `json:"items"`
				Total int                          `json:"total"`
			}
			read("/api/v1/analytics?"+test.query, &analytics)
			read("/api/v1/conversations?limit=100&"+test.query, &conversations)
			if analytics.ConversationCount != test.want || conversations.Total != test.want || !analytics.ReadOnly || !analytics.Filtered {
				t.Fatalf("统计与下钻不一致: analytics=%+v total=%d", analytics, conversations.Total)
			}
			if analytics.FileCount != test.want || analytics.SourceFileCount != 5 {
				t.Fatalf("关联文件范围不正确: %+v", analytics)
			}
			var size int64
			for _, item := range conversations.Items {
				for _, path := range item.RolloutPaths {
					size += int64(len(original[filepath.Join(root, path)]))
				}
			}
			if analytics.TotalBytes != size {
				t.Fatalf("关联大小 %d，期望 %d", analytics.TotalBytes, size)
			}
		})
	}
	var all analyticsResponse
	read("/api/v1/analytics", &all)
	if all.Filtered || all.ConversationCount != 4 || all.FileCount != 5 || all.Undated != 1 || len(all.Daily) != 3 {
		t.Fatalf("完整快照被改变或未知日期口径有误: %+v", all)
	}
	for _, query := range []string{"from=2026-10-04&to=2026-10-01", "from=2026-02-30", "cwd_exact=invalid"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/analytics?"+query, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("无效筛选应为 400: %s -> %d", query, recorder.Code)
		}
	}
	for path, before := range original {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("统计查询改变了源文件: %s, %v", path, err)
		}
	}
}
