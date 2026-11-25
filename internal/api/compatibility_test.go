// 中文说明：通过临时合成数据验证兼容策略持久化、工作区隔离、实际修改门禁和执行中的策略互斥。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/platform"
	"codex-chronicle.local/internal/store"
)

func compatibilityPolicy(t *testing.T, s *Server, mode string) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(map[string]string{"root": s.currentRoot(), "policy": mode})
	return apiJSON(t, s.Handler(), http.MethodPut, "/api/v1/compatibility", string(data))
}
func configureCompatibility(t *testing.T, s *Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compatibility.json")
	if err := s.SetCompatibilityPath(path); err != nil {
		t.Fatal(err)
	}
	return path
}
func requireCompatibilitySaved(t *testing.T, s *Server, mode string) {
	t.Helper()
	response := compatibilityPolicy(t, s, mode)
	if response.Code != http.StatusOK {
		t.Fatalf("保存失败：%d %s", response.Code, response.Body.String())
	}
}

func TestCompatibilityPersistsPerWorkspaceAndExplainsMixedVersions(t *testing.T) {
	root := t.TempDir()
	for i, version := range []string{"0.999.0", "0.998.0", "0.999.0"} {
		data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":\"test-%d\",\"cli_version\":%q}}\n", i, version)
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("rollout-%d.jsonl", i)), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(root)
	defer s.Close()
	path := configureCompatibility(t, s)
	response := apiJSON(t, s.Handler(), http.MethodGet, "/api/v1/compatibility", "")
	var status struct {
		Mode, Reason, Policy string
		Versions             []string
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &status) != nil || status.Reason != "检测到多个历史版本" || status.Mode != "通用读取" || strings.Join(status.Versions, ",") != "0.998.0,0.999.0" {
		t.Fatalf("混合版本解释错误：%s", response.Body.String())
	}
	requireCompatibilitySaved(t, s, "readonly")
	reopened := NewServer(root)
	defer reopened.Close()
	if err := reopened.SetCompatibilityPath(path); err != nil {
		t.Fatal(err)
	}
	if reopened.requireSourceChanges(root) == nil {
		t.Fatal("重启后丢失只读策略")
	}
	other := NewServer(t.TempDir())
	defer other.Close()
	if err := other.SetCompatibilityPath(path); err != nil {
		t.Fatal(err)
	}
	if other.requireSourceChanges(other.currentRoot()) != nil {
		t.Fatal("策略串入另一个工作区")
	}
	requireCompatibilitySaved(t, other, "readonly")
	if err := reopened.SetCompatibilityPath(path); err != nil {
		t.Fatal(err)
	}
	if reopened.requireSourceChanges(root) == nil {
		t.Fatal("保存另一工作区时丢失原策略")
	}
	response = apiJSON(t, reopened.Handler(), http.MethodGet, "/api/v1/capabilities", "")
	if !strings.Contains(response.Body.String(), `"source_policy":"readonly"`) {
		t.Fatal(response.Body.String())
	}
}

func TestCompatibilityRejectsInvalidInputAndFailedPersistence(t *testing.T) {
	s := NewServer(t.TempDir())
	defer s.Close()
	if r := compatibilityPolicy(t, s, "readonly"); r.Code != 409 {
		t.Fatal("未配置存储时应拒绝保存", r.Body.String())
	}
	if err := s.SetCompatibilityPath(filepath.Join(s.currentRoot(), "compatibility.json")); err == nil {
		t.Fatal("允许策略写入源目录")
	}
	path := configureCompatibility(t, s)
	for _, body := range []string{`{"root":"/other","policy":"readonly"}`, fmt.Sprintf(`{"root":%q,"policy":"force"}`, s.currentRoot()), fmt.Sprintf(`{"root":%q,"policy":"readonly","unknown":true}`, s.currentRoot())} {
		r := apiJSON(t, s.Handler(), http.MethodPut, "/api/v1/compatibility", body)
		if r.Code != 400 && r.Code != 409 {
			t.Fatalf("接受了无效输入：%s", r.Body.String())
		}
	}
	// 用目录占据目标文件位置，模拟原子替换失败；不能只在内存中切成只读。
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if r := compatibilityPolicy(t, s, "readonly"); r.Code != 500 {
		t.Fatal(r.Body.String())
	}
	if s.sourcePolicyFor(s.currentRoot()).Mode != "automatic" {
		t.Fatal("保存失败仍切换了内存策略")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte(`{"broken":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCompatibilityPath(invalid); err == nil {
		t.Fatal("损坏配置被静默放行")
	}
}

func TestCompatibilityReadOnlyBlocksSourceEndpointsAndAllowsDerivedIndex(t *testing.T) {
	root := t.TempDir()
	original := writeIndexAPIFile(t, root, 3)
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewServerWithStore(root, db)
	defer s.Close()
	configureCompatibility(t, s)
	requireCompatibilitySaved(t, s, "readonly")
	handler := s.Handler()
	for _, path := range []string{"/api/v1/operations/recover", "/api/v1/maintenance/execute", "/api/v1/maintenance/recover", "/api/v1/archives/local/restore/execute", "/api/v1/archives/local/restore/recovery", "/api/v1/archives/example/restore/execute", "/api/v1/github/archives/example/restore/execute", "/api/v1/archives/example/restore/execute/", "/api/v1/github/archives/example/restore/execute//"} {
		r := apiJSON(t, handler, http.MethodPost, path, `{}`)
		if r.Code != 403 || !strings.Contains(r.Body.String(), "仅查看") {
			t.Fatalf("源修改未被策略阻止：%s %d %s", path, r.Code, r.Body.String())
		}
	}
	for _, path := range []string{"/api/v1/conversations", "/api/v1/compatibility"} {
		if r := apiJSON(t, handler, http.MethodGet, path, ""); r.Code != 200 {
			t.Fatal(path, r.Body.String())
		}
	}
	indexRequest(t, handler, http.MethodGet, "", nil)
	indexRequest(t, handler, http.MethodPut, "", map[string]any{"root": root, "settings": map[string]any{"budget_bytes": 16 << 20, "automatic": false}})
	indexRequest(t, handler, http.MethodPost, "/build", map[string]any{"root": root})
	finished := awaitIndex(t, handler)
	if finished.Task.Status != "completed" {
		t.Fatalf("只读策略阻断派生索引：%+v", finished.Task)
	}
	after, err := os.ReadFile(filepath.Join(root, "rollout-index.jsonl"))
	if err != nil || string(after) != string(original) {
		t.Fatal("派生索引改变了源文件", err)
	}
}

func TestCompatibilityPolicyInvalidatesOldExecutionConfirmation(t *testing.T) {
	for _, switchBack := range []bool{false, true} {
		t.Run(fmt.Sprint(switchBack), func(t *testing.T) {
			s, ids := executionFixture(t, 1)
			configureCompatibility(t, s)
			handler := s.Handler()
			confirmation := prepareExecution(t, handler, ids, false)
			requireCompatibilitySaved(t, s, "readonly")
			if switchBack {
				requireCompatibilitySaved(t, s, "automatic")
			}
			if r := submitExecution(t, handler, confirmation); r.Code != 409 {
				t.Fatalf("旧确认仍可使用：%d %s", r.Code, r.Body.String())
			}
			if _, err := os.Stat(filepath.Join(s.currentRoot(), "execution-00.jsonl")); err != nil {
				t.Fatal("旧确认修改了源", err)
			}
			if switchBack {
				fresh := prepareExecution(t, handler, ids, false)
				r := submitExecution(t, handler, fresh)
				if r.Code != 202 {
					t.Fatal(r.Body.String())
				}
				var task executionTask
				_ = json.Unmarshal(r.Body.Bytes(), &task)
				if result := awaitExecution(t, handler, task.ID); result.State != "completed" {
					t.Fatalf("新确认执行失败：%+v", result)
				}
			} else {
				r := executionJSON(t, handler, "/api/v1/operations/batch/preview", batchOperationRequest{ConversationIDs: ids, Trash: true})
				if r.Code != 400 || !strings.Contains(r.Body.String(), "仅查看") {
					t.Fatal(r.Body.String())
				}
			}
		})
	}
}

func TestCompatibilityCannotSwitchDuringAcceptedSourceTask(t *testing.T) {
	s, ids := executionFixture(t, 1)
	configureCompatibility(t, s)
	directory, err := platform.NewDirectoryTrash(s.lifecycle.TrashRoot)
	if err != nil {
		t.Fatal(err)
	}
	trash := &controlledTrash{TrashProvider: directory, entered: make(chan struct{}), release: make(chan struct{}), fail: true}
	mutation, err := compat.NewSyntheticSQLiteMutation(filepath.Join(s.lifecycle.JournalRoot, "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	s.lifecycle.ControlPlaneMutation = mutation
	s.lifecycle.TrashProvider = trash
	handler := s.Handler()
	confirmation := prepareExecution(t, handler, ids, true)
	r := submitExecution(t, handler, confirmation)
	if r.Code != 202 {
		t.Fatal(r.Body.String())
	}
	released := false
	defer func() {
		if !released {
			close(trash.release)
		}
	}()
	select {
	case <-trash.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("未进入后台执行")
	}
	if r := compatibilityPolicy(t, s, "readonly"); r.Code != 409 {
		t.Fatal("运行中允许切成只读", r.Body.String())
	}
	close(trash.release)
	released = true
	var task executionTask
	_ = json.Unmarshal(r.Body.Bytes(), &task)
	if result := awaitExecution(t, handler, task.ID); result.State != "needs_review" {
		t.Fatalf("模拟失败未按预期进入人工复核：%+v", result)
	}
	// 任务终态写入和 defer 释放计数之间只有极短窗口，等待释放后再检查保存。
	deadline := time.Now().Add(time.Second)
	for {
		s.compatibilityMu.Lock()
		active := s.sourceChanges[s.currentRoot()]
		s.compatibilityMu.Unlock()
		if active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("操作计数未释放")
		}
		time.Sleep(time.Millisecond)
	}
	requireCompatibilitySaved(t, s, "readonly")
	if release, err := s.beginSourceChanges(s.currentRoot()); err == nil {
		release()
		t.Fatal("只读时仍能开始源修改")
	}
}

func TestCompatibilityReadOnlyStillExportsBackup(t *testing.T) {
	s, id, relative, original := cloudAPIFixture(t)
	configureCompatibility(t, s)
	requireCompatibilitySaved(t, s, "readonly")
	preview := inspectCloudAPI(t, s, id)
	r := githubSettingsRequest(t, s.Handler(), "POST", "/api/v1/backups/start", map[string]any{"preview_id": preview.ID, "confirm_upload": true}, 202)
	var task cloudTask
	_ = json.Unmarshal(r.Body.Bytes(), &task)
	result := awaitCloud(t, s, task.ID, func(task cloudTask) bool { return task.State == "completed" || task.State == "failed" })
	if result.State != "completed" {
		t.Fatalf("只读策略误挡备份导出：%+v", result)
	}
	after, err := os.ReadFile(filepath.Join(s.currentRoot(), relative))
	if err != nil || string(after) != string(original) {
		t.Fatal("备份改变源文件", err)
	}
}

func TestCompatibilityArchiveRestoreUsesServerReviewedAction(t *testing.T) {
	s := NewServer(t.TempDir())
	defer s.Close()
	configureCompatibility(t, s)
	requireCompatibilitySaved(t, s, "readonly")
	task := cloudTask{ID: "backup-0123456789abcdef01234567", SourceRoot: s.currentRoot()}
	review := archiveManagementReview{ID: "review", TaskID: task.ID, SourceRoot: task.SourceRoot, Action: "restore", Code: "123456", ExpiresAt: time.Now().Add(time.Minute)}
	s.cloud = &cloudBackupState{root: t.TempDir(), tasks: map[string]cloudTask{task.ID: task}, reviews: map[string]archiveManagementReview{review.ID: review}}
	r := executionJSON(t, s.Handler(), "/api/v1/archive-center/"+task.ID+"/execute", map[string]any{"preview_id": review.ID, "confirmation_code": review.Code, "confirm": true, "action": "delete_local"})
	if r.Code != 403 || !strings.Contains(r.Body.String(), "仅查看") {
		t.Fatalf("用客户端 action 绕过恢复限制：%d %s", r.Code, r.Body.String())
	}
}
