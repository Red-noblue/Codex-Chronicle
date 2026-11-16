// 中文说明：验证阅读 API 的真实来源边界、长消息搜索、失效锚点及只读行为。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
	"codex-chronicle.local/internal/store"
)

func TestReaderAPIUsesSourceAnchorsAndSearchesBeyondPreview(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"reader-api\"}}\n"
	for i := 0; i < 180; i++ {
		text := fmt.Sprintf("提问 %d", i)
		if i == 140 {
			text = strings.Repeat("长", 30000) + "专用末尾关键词"
		}
		data, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"text": text}}}})
		content += string(data) + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	handler := NewServer(root).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		return rr
	}
	list := get("/api/v1/conversations")
	var conversations struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &conversations); err != nil || len(conversations.Items) != 1 {
		t.Fatalf("list %s", list.Body)
	}
	endpoint := "/api/v1/conversations/" + conversations.Items[0].ID + "/reader"
	first := get(endpoint)
	var page reader.Page
	if first.Code != 200 || json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 80 || page.Items[0].RelativePath != "rollout.jsonl" {
		t.Fatalf("reader %d %s", first.Code, first.Body)
	}
	anchor := page.Items[0]
	res := get(endpoint + "?path=rollout.jsonl&start=" + fmt.Sprint(anchor.StartOffset) + "&anchor=changed")
	if res.Code != http.StatusConflict {
		t.Fatal("stale anchor accepted", res.Code)
	}
	res = get(endpoint + "?path=../secret&start=0")
	if res.Code != http.StatusConflict {
		t.Fatal("foreign path allowed", res.Code)
	}
	cursor := ""
	found := false
	for i := 0; i < 5; i++ {
		res = get(endpoint + "/search?q=" + url.QueryEscape("专用末尾关键词") + "&cursor=" + url.QueryEscape(cursor))
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil {
			t.Fatalf("search %d %s", res.Code, res.Body)
		}
		for _, item := range page.Items {
			if strings.Contains(item.Text, "专用末尾关键词") {
				found = true
			}
		}
		cursor = page.Next
		if !page.HasMore {
			break
		}
	}
	if !found {
		t.Fatal("search missed truncated message tail")
	}
	res = get(endpoint + "?tail=true")
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &page) != nil || page.Items[len(page.Items)-1].Text != "提问 179" {
		t.Fatalf("tail %d %s", res.Code, res.Body)
	}
	after, _ := os.ReadFile(path)
	if string(after) != content {
		t.Fatal("reader modified source")
	}
}

func TestGlobalEffectiveSearchRetainsDistinctIdenticalMessages(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint("indexed=", indexed), func(t *testing.T) {
			root := t.TempDir()
			data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"repeated-search\"}}\n" +
				strings.Repeat("{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"same-real-question\"}}\n", 2)
			if err := os.WriteFile(filepath.Join(root, "repeat.jsonl"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			var db *store.Store
			if indexed {
				var err error
				db, err = store.Open(t.Context(), ":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
			}
			handler := NewServerWithStore(root, db).Handler()
			if indexed {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/catalog/sync", nil))
				if rr.Code != 200 {
					t.Fatal(rr.Body)
				}
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/search?q=same-real-question", nil))
			var result struct {
				Hits []domain.SearchHit `json:"hits"`
			}
			if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &result) != nil || len(result.Hits) != 2 || result.Hits[0].StartOffset == result.Hits[1].StartOffset {
				t.Fatalf("distinct repeated messages collapsed: %d %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestReaderAPIIndexedSearchRechecksSourceAndHonorsRole(t *testing.T) {
	root := t.TempDir()
	data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"indexed-reader\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"text\":\"unique-reader-keyword\"}]}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := NewServerWithStore(root, db).Handler()
	ids := apiConversationIDs(t, handler)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("POST", "/api/v1/catalog/sync", nil))
	if rr.Code != 200 {
		t.Fatal(rr.Body)
	}
	endpoint := "/api/v1/conversations/" + ids[0] + "/reader/search?q=unique-reader-keyword"
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", endpoint+"&role=assistant", nil))
	var page reader.Page
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Items) != 1 || !page.HasMore {
		t.Fatalf("indexed search: %d %s", rr.Code, rr.Body)
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("search may be cached")
	}
	cursor, err := reader.DecodePosition(page.Next)
	if err != nil || cursor.Source != "rollout.jsonl" || cursor.ProjectionStamp == "" {
		t.Fatal("indexed cursor lost source", cursor, err)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", endpoint+"&role=user", nil))
	page = reader.Page{}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Items) != 0 || page.HasMore {
		t.Fatalf("role filter: %d %s", rr.Code, rr.Body)
	}
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte(strings.ReplaceAll(data, "unique-reader-keyword", "source-text-changed")), 0600); err != nil {
		t.Fatal(err)
	}
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest("GET", endpoint, nil))
	page = reader.Page{}
	if rr.Code != 200 || json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Items) != 0 {
		t.Fatalf("stale index trusted: %d %s", rr.Code, rr.Body)
	}
}

// 中文说明：正文、普通检索与摘要排除撤回记录，原始技术检索仍能回溯证据。
func TestReaderEffectiveHistoryFilesAndTechnicalEvidence(t *testing.T) {
	root := t.TempDir()
	line := func(kind string, payload any) string {
		b, _ := json.Marshal(map[string]any{"type": kind, "payload": payload})
		return string(b) + "\n"
	}
	msg := func(text string) string {
		return line("response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]string{"text": text}}})
	}
	data := line("session_meta", map[string]any{"id": "semantic-api"}) + msg("withdrawn_unique_secret") + line("event_msg", map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": map[string]any{"total_tokens": 1234}}}) + line("event_msg", map[string]any{"type": "thread_rolled_back", "num_turns": 1}) + msg("current_question") +
		line("response_item", map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "edit", "input": "*** Begin Patch\n*** Update File: src/example.go\n@@\n-old\n+new\n*** End Patch"}) +
		line("response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "edit", "output": "Process exited with code 0\noutput_unique_keyword"})
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler := NewServerWithStore(root, db).Handler()
	ids := apiConversationIDs(t, handler)
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 200 {
			t.Fatalf("%d %s", rr.Code, rr.Body)
		}
		return rr
	}
	endpoint := "/api/v1/conversations/" + ids[0] + "/reader"
	list := get("/api/v1/conversations")
	var inventory struct {
		Items []struct {
			Title string `json:"title"`
			First string `json:"first_user_text"`
			Usage struct {
				Total int64 `json:"total_tokens"`
			} `json:"token_usage"`
		} `json:"items"`
	}
	if json.Unmarshal(list.Body.Bytes(), &inventory) != nil || len(inventory.Items) != 1 || inventory.Items[0].Title != "current_question" || inventory.Items[0].First != "current_question" || inventory.Items[0].Usage.Total != 1234 {
		t.Fatal("list metadata disagrees with effective history or erased token usage", list.Body)
	}
	rr := get(endpoint)
	var page reader.Page
	if json.Unmarshal(rr.Body.Bytes(), &page) != nil || len(page.Items) != 2 || page.Items[0].Text != "current_question" || page.Items[1].Status != "completed" {
		t.Fatal(rr.Body)
	}
	var overview struct {
		Files []reader.FileEntry `json:"files"`
		Total int                `json:"total"`
	}
	rr = get(endpoint + "?overview=files")
	if json.Unmarshal(rr.Body.Bytes(), &overview) != nil || overview.Total != 1 || overview.Files[0].Status != "completed" {
		t.Fatal(rr.Body)
	}
	rr = get(endpoint + "/search?q=withdrawn_unique_secret")
	if strings.Contains(rr.Body.String(), `"text":"withdrawn_unique_secret"`) {
		t.Fatal("reader search leaked rollback", rr.Body)
	}
	for _, query := range []string{"withdrawn_unique_secret", "output_unique_keyword"} {
		rr = get("/api/v1/search?q=" + query)
		var hits struct {
			Hits []struct {
				Snippet string `json:"snippet"`
				Start   int64  `json:"start"`
			} `json:"hits"`
		}
		if json.Unmarshal(rr.Body.Bytes(), &hits) != nil {
			t.Fatal(rr.Body)
		}
		if query == "withdrawn_unique_secret" && len(hits.Hits) != 0 {
			t.Fatal("global search leaked rollback", rr.Body)
		}
		if query == "output_unique_keyword" && (len(hits.Hits) != 1 || !strings.Contains(hits.Hits[0].Snippet, query)) {
			t.Fatal("paired output missing", rr.Body)
		}
	}
	rr = get("/api/v1/search?q=withdrawn_unique_secret&scope=technical")
	if !strings.Contains(rr.Body.String(), "withdrawn_unique_secret") || !strings.Contains(rr.Body.String(), `"count":1`) {
		t.Fatal("technical evidence missing", rr.Body)
	}
	rr = get("/api/v1/conversations/" + ids[0] + "/summary")
	if strings.Contains(rr.Body.String(), "withdrawn_unique_secret") || !strings.Contains(rr.Body.String(), "current_question") {
		t.Fatal("summary disagrees with reader", rr.Body)
	}
	after, _ := os.ReadFile(path)
	if string(after) != data {
		t.Fatal("source changed")
	}
}
