// 中文说明：验证一次性确认、十项批次、计划绑定、重复提交、停止与重启；所有源数据均为临时合成样本。
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/compat/authorizedlive"
	"codex-chronicle.local/internal/platform"
)

func executionJSON(t *testing.T, handler http.Handler, path string, input any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return apiJSON(t, handler, http.MethodPost, path, string(data))
}

func executionFixture(t *testing.T, count int) (*Server, []string) {
	t.Helper()
	root := t.TempDir()
	ids := make([]string, count)
	for i := range ids {
		raw := fmt.Sprintf("execution-%02d", i)
		ids[i] = compat.StableConversationID(raw)
		data := fmt.Sprintf("{\"type\":\"session_meta\",\"payload\":{\"id\":%q}}\n", raw)
		if err := os.WriteFile(filepath.Join(root, raw+".jsonl"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	server := NewServerWithOptions(root, nil, &LifecycleOptions{Enabled: true, FixtureOnly: true,
		Descriptor: compat.SyntheticFixtureDescriptor(), JournalRoot: filepath.Join(t.TempDir(), "journal"),
		TrashRoot: filepath.Join(t.TempDir(), "trash"), ActivityOptions: &activity.Options{Interval: time.Millisecond, DisableProcessProbe: true, Conservative: false}})
	t.Cleanup(server.Close)
	return server, ids
}

func prepareExecution(t *testing.T, handler http.Handler, ids []string, trash bool) executionConfirmation {
	t.Helper()
	response := executionJSON(t, handler, "/api/v1/operations/batch/preview", batchOperationRequest{ConversationIDs: ids, Trash: trash, PermanentDelete: !trash})
	if response.Code != http.StatusOK {
		t.Fatalf("预览失败: %d %s", response.Code, response.Body.String())
	}
	var preview batchOperationResponse
	if err := json.Unmarshal(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	response = executionJSON(t, handler, "/api/v1/operations/confirmations", map[string]string{"preview_id": preview.PreviewID})
	if response.Code != http.StatusOK {
		t.Fatalf("确认失败: %d %s", response.Code, response.Body.String())
	}
	var confirmation executionConfirmation
	if err := json.Unmarshal(response.Body.Bytes(), &confirmation); err != nil {
		t.Fatal(err)
	}
	return confirmation
}

func TestArchivePreviewRequiresExplicitDestination(t *testing.T) {
	response := executionJSON(t, NewServer(t.TempDir()).Handler(), "/api/v1/operations/batch/preview", batchOperationRequest{
		ConversationIDs: []string{"conversation"}, Archive: true,
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "归档至少选择一个保存位置") {
		t.Fatalf("未选择归档位置时应拒绝预览: status=%d body=%s", response.Code, response.Body.String())
	}
}

func submitExecution(t *testing.T, handler http.Handler, confirmation executionConfirmation) *httptest.ResponseRecorder {
	t.Helper()
	return executionJSON(t, handler, "/api/v1/operations/executions", executionRequest{ConfirmationID: confirmation.ID, Code: confirmation.Code, AdditionalCode: confirmation.AdditionalCode})
}

func awaitExecution(t *testing.T, handler http.Handler, id string) executionTask {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		response := apiJSON(t, handler, http.MethodGet, "/api/v1/operations/executions/"+id, "")
		var task executionTask
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &task) != nil {
			t.Fatalf("任务查询失败: %s", response.Body.String())
		}
		if task.State != "running" {
			return task
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("任务没有在预期时间结束")
	return executionTask{}
}

func TestConfirmedExecutionBatchBoundaries(t *testing.T) {
	for _, count := range []int{1, 10, 11, 21} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			server, ids := executionFixture(t, count+1)
			handler := server.Handler()
			confirmation := prepareExecution(t, handler, ids[:count], false)
			if len(confirmation.Code) != 6 || confirmation.BatchSize != 10 || confirmation.Total != count || (confirmation.AdditionalCode != "") != (count > 10) {
				t.Fatalf("确认强度或批次错误: %+v", confirmation)
			}
			if confirmation.AdditionalCode != "" && (len(confirmation.AdditionalCode) != 6 || confirmation.Code == confirmation.AdditionalCode) {
				t.Fatal("第二组数字不独立")
			}
			response := submitExecution(t, handler, confirmation)
			if response.Code != http.StatusAccepted {
				t.Fatalf("提交失败: %s", response.Body.String())
			}
			var started executionTask
			_ = json.Unmarshal(response.Body.Bytes(), &started)
			finished := awaitExecution(t, handler, started.ID)
			if finished.State != "completed" || finished.Completed != count || finished.CurrentBatch != (count+9)/10 {
				t.Fatalf("任务结果不符: %+v", finished)
			}
			for i := 0; i < count; i++ {
				if _, err := os.Stat(filepath.Join(server.root, fmt.Sprintf("execution-%02d.jsonl", i))); !os.IsNotExist(err) {
					t.Fatal("目标未删除")
				}
			}
			if _, err := os.Stat(filepath.Join(server.root, fmt.Sprintf("execution-%02d.jsonl", count))); err != nil {
				t.Fatal("非目标被改变")
			}
			replayed := submitExecution(t, handler, confirmation)
			if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), started.ID) {
				t.Fatal("网络重试没有返回同一任务")
			}
			entries, _ := os.ReadDir(server.lifecycle.JournalRoot)
			journals := 0
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
					journals++
				}
			}
			if journals != count {
				t.Fatalf("journal 数量不符或发生重放: %d", journals)
			}
		})
	}
}

func TestConfirmationWrongExpiredAndLegacyBypass(t *testing.T) {
	server, ids := executionFixture(t, 11)
	handler := server.Handler()
	for _, path := range []string{"/api/v1/operations/execute", "/api/v1/operations/batch/execute"} {
		response := executionJSON(t, handler, path, map[string]any{"conversation_id": ids[0], "permanent_delete": true, "confirmation_code": "123456"})
		if response.Code != http.StatusBadRequest {
			t.Fatal("旧接口绕过了一次性确认")
		}
	}
	confirmation := prepareExecution(t, handler, ids, false)
	wrong := confirmation
	wrong.AdditionalCode = ""
	if response := submitExecution(t, handler, wrong); response.Code != http.StatusConflict {
		t.Fatal("超过十项没有强制第二次确认")
	}
	if response := submitExecution(t, handler, confirmation); response.Code != http.StatusConflict {
		t.Fatal("错误后确认未失效")
	}
	confirmation = prepareExecution(t, handler, ids, false)
	server.executionMu.Lock()
	server.executionConfirmations[confirmation.ID].ExpiresAt = time.Now().Add(-time.Second)
	server.executionMu.Unlock()
	if response := submitExecution(t, handler, confirmation); response.Code != http.StatusConflict {
		t.Fatal("过期确认被接受")
	}
	entries, _ := os.ReadDir(server.root)
	if len(entries) != 11 {
		t.Fatal("错误确认改变了源文件")
	}
}

func TestConfirmationBindsPlanAndDestination(t *testing.T) {
	for _, change := range []string{"file-before-confirm", "file-before-execute", "destination", "new-asset"} {
		t.Run(change, func(t *testing.T) {
			server, ids := executionFixture(t, 1)
			handler := server.Handler()
			confirmation := prepareExecution(t, handler, ids, true)
			switch change {
			case "destination":
				server.lifecycle.TrashRoot = filepath.Join(t.TempDir(), "changed")
			case "new-asset":
				data := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"execution-00\"}}\n")
				if err := os.WriteFile(filepath.Join(server.root, "second-rollout.jsonl"), data, 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				file, err := os.OpenFile(filepath.Join(server.root, "execution-00.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = file.WriteString("{\"type\":\"event_msg\",\"payload\":{}}\n")
				_ = file.Close()
			}
			if change == "file-before-confirm" {
				response := executionJSON(t, handler, "/api/v1/operations/confirmations", map[string]string{"preview_id": confirmation.PreviewID})
				if response.Code != http.StatusConflict {
					t.Fatal("文件变化后仍签发确认")
				}
				return
			}
			response := submitExecution(t, handler, confirmation)
			if response.Code == http.StatusAccepted {
				var started executionTask
				_ = json.Unmarshal(response.Body.Bytes(), &started)
				finished := awaitExecution(t, handler, started.ID)
				if finished.State != "needs_review" || finished.Completed != 0 {
					t.Fatalf("新关联没有阻断: %+v", finished)
				}
			} else if response.Code != http.StatusConflict {
				t.Fatalf("异常状态: %s", response.Body.String())
			}
			if _, err := os.Stat(filepath.Join(server.root, "execution-00.jsonl")); err != nil {
				t.Fatal("变化后仍删除了目标")
			}
		})
	}
}

type controlledTrash struct {
	platform.TrashProvider
	entered chan struct{}
	release chan struct{}
	fail    bool
	once    sync.Once
}

func (p *controlledTrash) Move(ctx context.Context, id, root, relative string) (platform.TrashReceipt, error) {
	p.once.Do(func() { close(p.entered); <-p.release })
	if p.fail {
		return platform.TrashReceipt{}, fmt.Errorf("模拟回收站失败")
	}
	return p.TrashProvider.Move(ctx, id, root, relative)
}

func TestExecutionStopFailureConcurrencyAndRestart(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("failure=%v", failure), func(t *testing.T) {
			server, ids := executionFixture(t, 3)
			// 提供共享索引，验证完整控制面的按行删除和停止边界。
			var indexData strings.Builder
			for i := range ids {
				fmt.Fprintf(&indexData, "{\"id\":\"execution-%02d\"}\n", i)
			}
			if err := os.WriteFile(filepath.Join(server.root, "session_index.jsonl"), []byte(indexData.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			mutation, err := compat.NewSyntheticSQLiteMutation(filepath.Join(server.lifecycle.JournalRoot, "snapshots"))
			if err != nil {
				t.Fatal(err)
			}
			server.lifecycle.ControlPlaneMutation = mutation
			directory, _ := platform.NewDirectoryTrash(server.lifecycle.TrashRoot)
			trash := &controlledTrash{TrashProvider: directory, entered: make(chan struct{}), release: make(chan struct{}), fail: failure}
			server.lifecycle.TrashProvider = trash
			handler := server.Handler()
			confirmation := prepareExecution(t, handler, ids, true)
			response := submitExecution(t, handler, confirmation)
			if response.Code != http.StatusAccepted {
				t.Fatalf("提交失败: %s", response.Body.String())
			}
			var started executionTask
			_ = json.Unmarshal(response.Body.Bytes(), &started)
			select {
			case <-trash.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("没有进入回收站动作")
			}
			var group sync.WaitGroup
			for i := 0; i < 4; i++ {
				group.Go(func() {
					if replay := submitExecution(t, handler, confirmation); replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), started.ID) {
						t.Error("并发重试重复创建任务")
					}
				})
			}
			group.Wait()
			if !failure {
				response := apiJSON(t, handler, http.MethodPost, "/api/v1/operations/executions/"+started.ID+"/stop", "")
				if response.Code != http.StatusOK {
					t.Fatal("停止请求失败")
				}
			}
			close(trash.release)
			finished := awaitExecution(t, handler, started.ID)
			wantState, wantCompleted := "stopped", 1
			if failure {
				wantState, wantCompleted = "needs_review", 0
			}
			if finished.State != wantState || finished.Completed != wantCompleted {
				t.Fatalf("停止或失败仍执行后续项: %+v", finished)
			}
			for i := 1; i < 3; i++ {
				if _, err := os.Stat(filepath.Join(server.root, fmt.Sprintf("execution-%02d.jsonl", i))); err != nil {
					t.Fatal("后续项被操作")
				}
			}
			// 模拟落盘记录处于 running 的进程中断；新实例只读加载，不触发源操作。
			finished.State = "running"
			if err := server.saveExecutionTask(&finished); err != nil {
				t.Fatal(err)
			}
			restarted := NewServerWithOptions(server.root, nil, server.lifecycle)
			t.Cleanup(restarted.Close)
			reloaded := awaitExecution(t, restarted.Handler(), started.ID)
			if reloaded.State != "needs_review" || reloaded.Completed != wantCompleted {
				t.Fatal("重启自动续删或丢失进度")
			}
		})
	}
}

func TestRestrictedExecutionDoesNotExpandAuthorization(t *testing.T) {
	server, ids := executionFixture(t, 1)
	server.lifecycle.FixtureOnly = false
	server.lifecycle.AuthorizedLive = &authorizedlive.Config{}
	handler := server.Handler()
	preview := executionJSON(t, handler, "/api/v1/operations/batch/preview", batchOperationRequest{ConversationIDs: ids, Trash: true})
	if preview.Code != http.StatusConflict || !strings.Contains(preview.Body.String(), "未获得本机真实执行授权") {
		t.Fatalf("越权预览未阻断: %s", preview.Body.String())
	}
	entries, _ := os.ReadDir(server.root)
	if len(entries) != 1 {
		t.Fatal("未授权目标被改变")
	}
	if server.lifecycleWriteEnabled() || server.executionAllowed(server.root) {
		t.Fatal("受限权限扩展到了通用真实写")
	}
}

func TestConfirmedExecutionWorkspaceIsolationAndCorruptRecord(t *testing.T) {
	server, ids := executionFixture(t, 1)
	handler := server.Handler()
	confirmation := prepareExecution(t, handler, ids, false)
	started := submitExecution(t, handler, confirmation)
	if started.Code != http.StatusAccepted {
		t.Fatal(started.Body.String())
	}
	var task executionTask
	_ = json.Unmarshal(started.Body.Bytes(), &task)
	_ = awaitExecution(t, handler, task.ID)
	server.workspaceMu.Lock()
	server.workspaceReg.Workspaces[0].Root = t.TempDir()
	server.workspaceMu.Unlock()
	if response := submitExecution(t, handler, confirmation); response.Code != http.StatusConflict || strings.Contains(response.Body.String(), task.ID) {
		t.Fatal("旧工作区确认泄露或重新启动了原任务")
	}

	other, otherIDs := executionFixture(t, 1)
	otherConfirmation := prepareExecution(t, other.Handler(), otherIDs, false)
	path := filepath.Join(other.executionTaskRoot(), "execution-batch-corrupt.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	other.loadExecutionTasks()
	if response := submitExecution(t, other.Handler(), otherConfirmation); response.Code != http.StatusConflict {
		t.Fatal("损坏执行记录未阻断新操作")
	}
	response := apiJSON(t, other.Handler(), http.MethodGet, "/api/v1/operations/executions", "")
	if !strings.Contains(response.Body.String(), "execution-batch-corrupt.json") {
		t.Fatal("没有向用户报告损坏记录")
	}
	if _, err := os.Stat(filepath.Join(other.root, "execution-00.jsonl")); err != nil {
		t.Fatal("损坏记录后目标仍被修改")
	}
}

func TestExecutionExpiredPreparedPackageCleanupRetainsRecovery(t *testing.T) {
	server, _ := executionFixture(t, 1)
	for _, scenario := range []string{"unused", "referenced", "started", "changed"} {
		work := filepath.Join(server.lifecycle.JournalRoot, "authorized-batch-"+scenario)
		if err := os.MkdirAll(work, 0o700); err != nil {
			t.Fatal(err)
		}
		plan := []byte(`{"kind":"synthetic-cleanup-check"}`)
		hash := sha256.Sum256(plan)
		digest := hex.EncodeToString(hash[:])
		files := map[string][]byte{"plan.json": plan, "plan.sha256": []byte(digest), "journal.json": []byte(`{"phase":"prepared","receipts":{},"steps":[]}`)}
		if scenario == "started" {
			files["journal.json"] = []byte(`{"phase":"needs_review","receipts":{},"steps":["trash"]}`)
		}
		if scenario == "changed" {
			files["plan.json"] = []byte("changed")
		}
		for name, data := range files {
			if err := os.WriteFile(filepath.Join(work, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if scenario == "referenced" {
			server.executionTasks["retain"] = &executionTask{RecoveryPath: work}
		}
		server.executionReviews[scenario] = &executionReview{RestrictedWork: work, RestrictedDigest: digest, ExpiresAt: time.Now().Add(-time.Second)}
		server.pruneExecutionReviewsLocked()
		_, err := os.Stat(work)
		if (scenario == "unused" && !os.IsNotExist(err)) || (scenario != "unused" && err != nil) {
			t.Fatalf("清理边界不符：%s %v", scenario, err)
		}
	}
}

// executeReviewedAPI 将旧集成回归迁移到确认与异步任务，同时保留原断言关注的 saga 结果。
func executeReviewedAPI(t *testing.T, handler http.Handler, executeURL, body string) *httptest.ResponseRecorder {
	t.Helper()
	preview := apiJSON(t, handler, http.MethodPost, strings.Replace(executeURL, "/execute", "/preview", 1), body)
	if preview.Code != http.StatusOK {
		return preview
	}
	var result struct {
		PreviewID string `json:"preview_id"`
	}
	_ = json.Unmarshal(preview.Body.Bytes(), &result)
	confirmationResponse := executionJSON(t, handler, "/api/v1/operations/confirmations", map[string]string{"preview_id": result.PreviewID})
	if confirmationResponse.Code != http.StatusOK {
		return confirmationResponse
	}
	var confirmation executionConfirmation
	_ = json.Unmarshal(confirmationResponse.Body.Bytes(), &confirmation)
	response := executionJSON(t, handler, executeURL, executionRequest{ConfirmationID: confirmation.ID, Code: confirmation.Code, AdditionalCode: confirmation.AdditionalCode})
	if response.Code != http.StatusAccepted {
		return response
	}
	var started executionTask
	_ = json.Unmarshal(response.Body.Bytes(), &started)
	finished := awaitExecution(t, handler, started.ID)
	status := http.StatusOK
	if finished.State != "completed" {
		status = http.StatusConflict
	}
	recorder := httptest.NewRecorder()
	if strings.Contains(executeURL, "/batch/") {
		writeJSON(recorder, status, finished.Preview)
	} else {
		item := finished.Preview.Items[0]
		writeJSON(recorder, status, map[string]any{"read_only": finished.Preview.ReadOnly, "blocked": finished.State != "completed", "error": finished.Error, "result": item.Result})
	}
	return recorder
}

// lifecycleJournalEntries 忽略新的任务记录子目录，只统计独立物理操作 journal。
func lifecycleJournalEntries(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	result := make([]os.DirEntry, 0)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			result = append(result, entry)
		}
	}
	return result, err
}
