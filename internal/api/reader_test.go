// 中文说明：验证阅读 API 的真实来源边界、长消息搜索、失效锚点及只读行为。
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/reader"
	"codex-chronicle.local/internal/store"
)

func TestSearchStreamScopesBeforeLimitAndReturnsUnloadedConversations(t *testing.T) {
	root := t.TempDir()
	write := func(id, cwd, stamp string, count int) {
		t.Helper()
		header := fmt.Sprintf("{\"timestamp\":%q,\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n", stamp, id, cwd)
		row := fmt.Sprintf("{\"timestamp\":%q,\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"共同关键词\"}}\n", stamp)
		if err := os.WriteFile(filepath.Join(root, "rollout-"+id+".jsonl"), []byte(header+strings.Repeat(row, count)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a", "/projects/unrelated", "2026-10-02T00:00:00Z", 170)
	write("z", "/projects/target", "2026-10-01T00:00:00Z", 2)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithStore(root, db)
	defer server.Close()
	handler := server.Handler()
	endpoint := "/api/v1/search?limit=3&conversation_cwd=target&q=" + url.QueryEscape("共同关键词")
	var expected []domain.SearchHit
	for _, mode := range []string{"live_scan", "archive_fts"} {
		if mode == "archive_fts" {
			indexRequest(t, handler, http.MethodPost, "/build", map[string]string{"root": root})
			if state := awaitIndex(t, handler); state.Task.Status != "completed" || state.Index.IndexedFiles != 2 {
				t.Fatalf("构建失败：%+v", state)
			}
		}
		stream := apiJSON(t, handler, http.MethodGet, endpoint+"&stream=1", "")
		if stream.Code != http.StatusOK || !stream.Flushed || !strings.Contains(stream.Header().Get("Content-Type"), "application/x-ndjson") {
			t.Fatalf("流式响应未启用：%d %v", stream.Code, stream.Header())
		}
		decoder := json.NewDecoder(bytes.NewReader(stream.Body.Bytes()))
		var events []searchStreamEvent
		for {
			var event searchStreamEvent
			if err := decoder.Decode(&event); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			events = append(events, event)
		}
		if len(events) < 4 || events[0].Type != "catalog" || events[1].Type != "search" || events[1].FilesTotal != 1 ||
			events[len(events)-1].Type != "done" || events[len(events)-1].Limited {
			t.Fatalf("进度或搜索范围错误：%s", stream.Body)
		}
		var hits []domain.SearchHit
		var items []domain.ConversationSummary
		for _, event := range events {
			hits = append(hits, event.Hits...)
			items = append(items, event.Items...)
		}
		if len(hits) != 2 || len(items) != 1 || items[0].SourceID != "z" || hits[0].ConversationID != items[0].ID ||
			hits[0].Line != 2 || hits[1].Line != 3 {
			t.Fatalf("未正确返回待阅读会话和字节命中：%s", stream.Body)
		}
		var body struct {
			Mode string             `json:"search_mode"`
			Hits []domain.SearchHit `json:"hits"`
		}
		if result := apiJSON(t, handler, http.MethodGet, endpoint, ""); result.Code != http.StatusOK ||
			json.Unmarshal(result.Body.Bytes(), &body) != nil || body.Mode != mode || !reflect.DeepEqual(body.Hits, hits) {
			t.Fatalf("普通响应与流式响应不一致：%s", result.Body)
		}
		for i := range hits {
			wantOrigin := domain.SearchMatchSource
			if mode == "archive_fts" {
				wantOrigin = domain.SearchMatchIndex
			}
			if hits[i].MatchOrigin != wantOrigin {
				t.Fatalf("搜索方式 %s 的命中来源错误：%+v", mode, hits[i])
			}
			hits[i].LiveConsistency, hits[i].SourceStatus, hits[i].ActivityStatus = "", "", ""
			hits[i].MatchOrigin = ""
		}
		if expected == nil {
			expected = hits
		} else if !reflect.DeepEqual(expected, hits) {
			t.Fatalf("建立索引改变了命中或定位：%v != %v", expected, hits)
		}
	}
	list := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations?limit=1", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), expected[0].ConversationID) {
		t.Fatalf("样本目标应在首个列表页之外：%s", list.Body)
	}
}

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

func TestGlobalSearchOnFreshInstallAndPastRejectedCandidates(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "rollout-fresh-search.jsonl")
	data := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"first-search\"}}\n" +
		strings.Repeat("{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"developer\",\"content\":[{\"text\":\"needle 同样的测试词\"}]}}\n", 510) +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"" +
		strings.Repeat("长", 6000) + "needle 跨片\"},{\"text\":\"段命中\"}]}}\n"
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithStore(root, db)
	defer server.Close()
	handler := server.Handler()
	for _, mode := range []string{"live_scan", "archive_fts"} {
		if mode == "archive_fts" {
			indexRequest(t, handler, http.MethodPost, "/build", map[string]string{"root": root})
			if state := awaitIndex(t, handler); state.Task.Status != "completed" || state.Index.IndexedFiles != 1 {
				t.Fatalf("构建失败：%+v", state)
			}
		}
		for _, query := range []string{"needle", "跨片段命中"} {
			response := apiJSON(t, handler, http.MethodGet, "/api/v1/search?limit=5&q="+url.QueryEscape(query), "")
			var body struct {
				Mode   string             `json:"search_mode"`
				Hits   []domain.SearchHit `json:"hits"`
				Capped bool               `json:"candidate_limit_reached"`
			}
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Mode != mode || body.Capped || len(body.Hits) != 1 || body.Hits[0].Line != 512 || !strings.Contains(body.Hits[0].Snippet, query) {
				t.Fatalf("查询 %q 未能跳过已隐藏的候选：%s", query, response.Body)
			}
		}
	}
	response := apiJSON(t, handler, http.MethodGet, "/api/v1/search?scope=technical&limit=5&q="+url.QueryEscape(`"role":"developer"`), "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"count":5`) {
		t.Fatalf("技术搜索未匹配原始 JSON 字段：%s", response.Body)
	}
	newSource := "{\"type\":\"session_meta\",\"payload\":{\"id\":\"another-search\"}}\n" +
		"{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"刚加入目录可搜\"}}\n"
	if err := os.WriteFile(filepath.Join(root, "rollout-new.jsonl"), []byte(newSource), 0600); err != nil {
		t.Fatal(err)
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/search?q="+url.QueryEscape("刚加入目录可搜"), "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"count":1`) || !strings.Contains(response.Body.String(), `"relative_path":"rollout-new.jsonl"`) {
		t.Fatalf("新会话未进入搜索范围：%s", response.Body)
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

func TestSearchVerifiesRollbackAppendedBeyondIndex(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout-live.jsonl")
	prefix := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"live-prefix\"}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"prefixwithdrawnword\"}]}}\n")
	if err := os.WriteFile(path, prefix, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewServerWithStore(root, db)
	defer s.Close()
	h := s.Handler()
	indexRequest(t, h, http.MethodPost, "/build", map[string]string{"root": root})
	if state := awaitIndex(t, h); state.Index.IndexedFiles != 1 {
		t.Fatalf("应先完整索引旧版本：%+v", state)
	}
	addition := "{\"type\":\"event_msg\",\"payload\":{\"type\":\"thread_rolled_back\",\"num_turns\":1}}\n" +
		"{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"text\":\"prefixvisibleword\"}]}}\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(addition)
	if closeErr := f.Close(); closeErr != nil || err != nil {
		t.Fatal(err, closeErr)
	}
	response := apiJSON(t, h, http.MethodGet, "/api/v1/search?q=prefix&stream=1", "")
	decoder := json.NewDecoder(response.Body)
	var hits []domain.SearchHit
	var end searchStreamEvent
	for {
		var event searchStreamEvent
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil || event.Type == "error" {
			t.Fatalf("搜索失败：%+v %v", event, err)
		}
		hits = append(hits, event.Hits...)
		end = event
	}
	if len(hits) != 1 || hits[0].Snippet != "prefixvisibleword" || hits[0].Line != 4 || hits[0].StartOffset <= int64(len(prefix)) {
		t.Fatalf("普通搜索应隐去被撤回的前缀命中，并保留原文后缀定位：%+v", hits)
	}
	if end.Type != "done" || end.PrefixBytes == nil || *end.PrefixBytes != int64(len(prefix))+1 {
		t.Fatalf("查询应使用经复核的旧版本前缀：%+v", end)
	}
}
