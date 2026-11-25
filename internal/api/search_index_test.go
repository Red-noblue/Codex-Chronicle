// 中文说明：用合成工作区验证索引控制 API、后台停止续建、设置持久化及源文件隔离。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/store"
	workspacepkg "codex-chronicle.local/internal/workspace"
)

type indexResponse struct {
	Available bool                    `json:"available"`
	ReadOnly  bool                    `json:"source_read_only"`
	Root      string                  `json:"root"`
	Index     store.SearchIndexStatus `json:"index"`
	Task      searchIndexTask         `json:"task"`
	Busy      bool                    `json:"busy"`
}

func indexRequest(t *testing.T, handler http.Handler, method, path string, body any) indexResponse {
	t.Helper()
	data := ""
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		data = string(encoded)
	}
	response := apiJSON(t, handler, method, "/api/v1/search-index"+path, data)
	if response.Code != http.StatusOK {
		t.Fatalf("索引请求失败 %s %s: %d %s", method, path, response.Code, response.Body.String())
	}
	var result indexResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func awaitIndex(t *testing.T, handler http.Handler) indexResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		result := indexRequest(t, handler, http.MethodGet, "", nil)
		if !result.Busy {
			return result
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("索引后台任务没有结束")
	return indexResponse{}
}

func writeIndexAPIFile(t *testing.T, root string, count int) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("{\"type\":\"session_meta\",\"payload\":{\"id\":\"index-control\"}}\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":\"第%d轮 中文检索 indexneedle%d %s\"}}\n", i, i, strings.Repeat("需要保留的完整合成正文。", 20))
	}
	source := []byte(b.String())
	if err := os.WriteFile(filepath.Join(root, "rollout-index.jsonl"), source, 0600); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestSearchIndexContentScopeSurvivesAppendAndReopen(t *testing.T) {
	root := t.TempDir()
	rollout := filepath.Join(root, "rollout-content-scope.jsonl")
	initial := `{"type":"session_meta","payload":{"id":"index-content"}}
{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"read1","input":"const a = await tools.exec_command({cmd: 'cat inputmarker.txt'}); text(a.output);"}}
{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"run1","arguments":"{\"cmd\":\"go test\"}"}}
{"type":"response_item","payload":{"type":"function_call_output","call_id":"run1","output":"commandresultmarker sharedneedle"}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"structured-read","command":["/bin/sh","-lc","sed -n 1p a.txt"],"parsed_cmd":[{"type":"read","path":"a.txt"}],"aggregated_output":"structuredreadmarker sharedneedle"}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"structured-change","status":"completed","changes":{"notes.txt":{"type":"add","content":"newfilemarker"},"oldname.txt":{"type":"update","unified_diff":"+renamedmarker","move_path":"newname.txt"},"other.txt":{"type":"update","unified_diff":"+modifiedmarker"},"obsolete.txt":{"type":"delete","content":"removedmarker"}}}}}
{"type":"response_item","payload":{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"finalanswerneedle"}]}}
{"type":"compacted","payload":{"message":"readablesummarymarker","replacement_history":[{"type":"compaction","encrypted_content":"ciphertextmarker"}]}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_tokens":9182537}}}
`
	if err := os.WriteFile(rollout, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "catalog.db")
	db, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithStore(root, db)
	t.Cleanup(func() { server.Close(); _ = db.Close() })
	query := func(handler http.Handler, scope, needle string, count int, origins ...domain.SearchMatchOrigin) {
		t.Helper()
		response := apiJSON(t, handler, http.MethodGet, "/api/v1/search?"+url.Values{"q": {needle}, "scope": {scope}}.Encode(), "")
		var result struct {
			Count int                `json:"count"`
			Hits  []domain.SearchHit `json:"hits"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Count != count {
			t.Fatalf("%s 搜索 %q：预期 %d 处命中，得到 %d %s", scope, needle, count, response.Code, response.Body.String())
		}
		if len(result.Hits) != count {
			t.Fatalf("命中清单与数量不一致：%+v", result)
		}
		for i, origin := range origins {
			if result.Hits[i].MatchOrigin != origin {
				t.Fatalf("%s 搜索 %q：第 %d 处应来自 %s，实际为 %+v", scope, needle, i+1, origin, result.Hits[i])
			}
		}
	}
	save := func(handler http.Handler, ids ...string) {
		t.Helper()
		indexRequest(t, handler, http.MethodPut, "", map[string]any{
			"root": root, "settings": store.SearchIndexSettings{BudgetBytes: 4 << 20, Content: &ids},
		})
	}
	build := func(handler http.Handler) indexResponse {
		t.Helper()
		indexRequest(t, handler, http.MethodPost, "/build", map[string]any{"root": root})
		result := awaitIndex(t, handler)
		if result.Task.Status != "completed" {
			t.Fatalf("构建失败：%+v", result.Task)
		}
		return result
	}

	handler := server.Handler()
	query(handler, "", "sharedneedle", 2, domain.SearchMatchSource, domain.SearchMatchSource)
	save(handler, "read.output")
	build(handler)
	query(handler, "", "sharedneedle", 2, domain.SearchMatchIndex, domain.SearchMatchSource)
	query(handler, "indexed", "sharedneedle", 1, domain.SearchMatchIndex)
	query(handler, "source", "sharedneedle", 2, domain.SearchMatchSource, domain.SearchMatchSource)
	query(handler, "indexed", "structuredreadmarker", 1)
	query(handler, "indexed", "commandresultmarker", 0)
	query(handler, "", "commandresultmarker", 1)

	// 重启后从已提交位置续建，输出在断点之后，所属工具调用在断点之前。
	server.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	newDB, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	newServer := NewServerWithStore(root, newDB)
	defer newDB.Close()
	defer newServer.Close()
	handler = newServer.Handler()
	added := `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"read1","output":"appendedreadmarker"}}
`
	if err := os.WriteFile(rollout, []byte(initial+added), 0600); err != nil {
		t.Fatal(err)
	}
	after := build(handler)
	if after.Index.Settings.Content == nil || strings.Join(*after.Index.Settings.Content, ",") != "read.output" ||
		after.Task.Progress.DocumentsAdded != 1 {
		t.Fatalf("重启后内容范围或续建位置不正确：%+v", after)
	}
	query(handler, "indexed", "appendedreadmarker", 1)
	query(handler, "indexed", "inputmarker.txt", 0)
	query(handler, "", "inputmarker.txt", 1)

	save(handler, "summary", "command.output", "final", "edit.create", "edit.rename")
	if status := indexRequest(t, handler, http.MethodGet, "", nil); !status.Index.RebuildRequired {
		t.Fatal("更换内容后应提示重新构建")
	}
	query(handler, "", "appendedreadmarker", 1)
	query(handler, "", "sharedneedle", 2, domain.SearchMatchSource, domain.SearchMatchSource)
	build(handler)
	query(handler, "indexed", "appendedreadmarker", 0)
	query(handler, "indexed", "commandresultmarker", 1)
	query(handler, "indexed", "readablesummarymarker", 1)
	query(handler, "indexed", "finalanswerneedle", 1)
	query(handler, "indexed", "newfilemarker", 1)
	query(handler, "indexed", "renamedmarker", 1)
	query(handler, "indexed", "modifiedmarker", 0)
	query(handler, "indexed", "removedmarker", 0)
	query(handler, "", "removedmarker", 1)
	query(handler, "indexed", "ciphertextmarker", 0)
	query(handler, "indexed", "9182537", 0)
	if source, err := os.ReadFile(rollout); err != nil || string(source) != initial+added {
		t.Fatalf("索引操作修改了源文件：%v", err)
	}
}

func TestSearchIndexControlPersistsAndLeavesSourcesUnchanged(t *testing.T) {
	root := t.TempDir()
	source := writeIndexAPIFile(t, root, 200)
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithStore(root, db)
	defer server.Close()
	handler := server.Handler()
	initial := indexRequest(t, handler, http.MethodGet, "", nil)
	if !initial.Available || !initial.ReadOnly || initial.Root != root || initial.Index.Settings.Automatic || initial.Index.Settings.BudgetBytes != store.DefaultSearchIndexBudget {
		t.Fatalf("初始状态不正确: %+v", initial)
	}
	for _, request := range []struct {
		method, path, body string
		code               int
	}{
		{http.MethodPut, "", `{"root":"wrong","settings":{"budget_bytes":1048576}}`, http.StatusConflict},
		{http.MethodPut, "", fmt.Sprintf(`{"root":%q,"settings":{"budget_bytes":0}}`, root), http.StatusBadRequest},
		{http.MethodPost, "/clear", fmt.Sprintf(`{"root":%q}`, root), http.StatusBadRequest},
		{http.MethodPost, "/build", fmt.Sprintf(`{"root":%q} {}`, root), http.StatusBadRequest},
		{http.MethodGet, "/build", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", "", http.StatusNotFound},
	} {
		response := apiJSON(t, handler, request.method, "/api/v1/search-index"+request.path, request.body)
		if response.Code != request.code {
			t.Fatalf("请求边界失效: %+v response=%d %s", request, response.Code, response.Body.String())
		}
	}
	settings := store.SearchIndexSettings{BudgetBytes: 32 << 20, Automatic: true}
	indexRequest(t, handler, http.MethodPut, "", map[string]any{"root": root, "settings": settings})
	indexRequest(t, handler, http.MethodPost, "/build", map[string]any{"root": root})
	result := awaitIndex(t, handler)
	if result.Task.Status != "completed" || result.Index.Documents != 201 || result.Index.IndexedFiles != 1 || result.Index.CatalogFiles != 1 {
		t.Fatalf("构建没有完成: %+v", result)
	}
	response := apiJSON(t, handler, http.MethodGet, "/api/v1/search?scope=technical&q="+url.QueryEscape("indexneedle199"), "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"count":1`) {
		t.Fatalf("构建后无法搜索: %s", response.Body.String())
	}
	indexRequest(t, handler, http.MethodPost, "/compact", map[string]any{"root": root})
	result = indexRequest(t, handler, http.MethodPost, "/clear", map[string]any{"root": root, "confirm": true})
	if result.Index.Documents != 0 || result.Index.Settings.Automatic || result.Index.CatalogFiles != 1 {
		t.Fatalf("清理边界失效: %+v", result)
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/search?scope=technical&q=indexneedle199", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"count":1`) {
		t.Fatalf("清理后没有回退只读搜索: %s", response.Body.String())
	}
	after, err := os.ReadFile(filepath.Join(root, "rollout-index.jsonl"))
	if err != nil || string(source) != string(after) {
		t.Fatal("索引操作修改了源文件", err)
	}
	server.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	persisted, err := db.SearchIndexSettings(t.Context())
	if err != nil || persisted.BudgetBytes != settings.BudgetBytes || persisted.Automatic {
		t.Fatalf("设置没有持久化: %+v %v", persisted, err)
	}
}

func TestSearchIndexStopAndResumeTask(t *testing.T) {
	root := t.TempDir()
	writeIndexAPIFile(t, root, 4000)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := NewServerWithStore(root, db)
	defer server.Close()
	handler := server.Handler()
	indexRequest(t, handler, http.MethodPost, "/build", map[string]any{"root": root})
	indexRequest(t, handler, http.MethodPost, "/stop", map[string]any{"root": root})
	result := awaitIndex(t, handler)
	if result.Task.Status != "paused" {
		t.Fatalf("停止构建无效: %+v", result)
	}
	indexRequest(t, handler, http.MethodPost, "/build", map[string]any{"root": root})
	result = awaitIndex(t, handler)
	if result.Task.Status != "completed" || result.Index.Documents != 4001 {
		t.Fatalf("停止后不能继续构建: %+v", result)
	}
}

func TestSearchIndexTaskIsolatesWorkspaceRemoval(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	server, err := NewServerWithWorkspaceConfig(root, nil, nil, filepath.Join(base, "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	other := filepath.Join(base, "other-source")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	definition, err := server.addWorkspace(t.Context(), workspacepkg.Definition{Root: other, ArchiveDB: filepath.Join(base, "other.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer server.CloseWorkspaceStores()
	// 模拟切换工作区后仍在运行的任务：移除其注册项不能关闭任务持有的数据库。
	server.indexTaskMu.Lock()
	server.indexTask = searchIndexTask{Root: other, Status: "running"}
	server.indexTaskCancel = func() {}
	server.indexTaskMu.Unlock()
	if _, err := server.removeWorkspace(t.Context(), definition.ID); err == nil {
		t.Fatal("仍在构建的工作区被移除了")
	}
	server.indexTaskMu.Lock()
	server.indexTaskCancel = nil
	server.indexTaskMu.Unlock()
	if _, err := server.removeWorkspace(t.Context(), definition.ID); err != nil {
		t.Fatal("停止任务后仍不能移除注册项", err)
	}
}
