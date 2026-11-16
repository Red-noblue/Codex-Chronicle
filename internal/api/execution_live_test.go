// 中文说明：核对真实执行器逐项 journal 的进度映射、永久删除后恢复状态和计划外 ID 拒绝，不操作真实数据。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/application"
	"codex-chronicle.local/internal/lifecycle"
)

func TestUnchangedLiveProgressDoesNotRewriteTaskOrMissStop(t *testing.T) {
	server, _ := executionFixture(t, 1)
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "journal.json"), []byte(`{"completed_ids":[],"current_batch":1,"current_item":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	task := &executionTask{ID: "execution-batch-unchanged", SourceRoot: server.root, State: "running", CurrentBatch: 1, CurrentItem: 1}
	if err := server.publishExecutionTask(task, false); err != nil {
		t.Fatal(err)
	}
	stamp := task.UpdatedAt
	for attempt := 0; attempt < 2; attempt++ {
		if err := server.syncLiveExecutionProgress(task, work); err != nil {
			t.Fatal(err)
		}
		if !task.UpdatedAt.Equal(stamp) {
			t.Fatal("未变化的进度仍重写持久任务")
		}
	}
	server.executionMu.Lock()
	server.executionTasks[task.ID].StopRequested = true
	server.executionMu.Unlock()
	if err := server.syncLiveExecutionProgress(task, work); err != nil {
		t.Fatal(err)
	}
	if !task.StopRequested || task.UpdatedAt.Equal(stamp) {
		t.Fatal("无进度变化时漏掉了用户的停止请求")
	}
	if _, err := os.Stat(filepath.Join(work, "stop-requested")); err != nil {
		t.Fatalf("停止请求未交给执行器: %v", err)
	}
}

func TestRunningRemovalInvalidatesListAfterEachCommittedItem(t *testing.T) {
	server, ids := executionFixture(t, 3)
	handler := server.Handler()
	if warm := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations", ""); warm.Code != http.StatusOK {
		t.Fatal(warm.Body.String())
	}
	// 只在合成源中模拟逐项已提交：批次仍 running，失败项和未执行项必须继续显示。
	task := &executionTask{ID: "execution-batch-progress", SourceRoot: server.root, State: "running",
		Strategy: operationPreviewRequest{Trash: true}, Preview: batchOperationResponse{Items: []batchOperationItem{
			{ConversationID: ids[0]}, {ConversationID: ids[1]}, {ConversationID: ids[2]},
		}}}
	if err := server.publishExecutionTask(task, false); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 2; index++ {
		if err := os.Remove(filepath.Join(server.root, fmt.Sprintf("execution-%02d.jsonl", index))); err != nil {
			t.Fatal(err)
		}
		task.Completed++
		task.Preview.Items[index].Completed = true
		if err := server.publishExecutionTask(task, false); err != nil {
			t.Fatal(err)
		}
		response := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations", "")
		var list struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &list) != nil || len(list.Items) != 2-index {
			t.Fatalf("整批结束前仍返回已提交删除的会话: %s", response.Body.String())
		}
		for _, item := range list.Items {
			for _, deleted := range ids[:index+1] {
				if item.ID == deleted {
					t.Fatalf("列表仍包含已提交目标: %s", deleted)
				}
			}
		}
	}
	previous, err := server.tasks.ReadReport(t.Context(), server.root, application.ScanOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	task.State, task.Error = "needs_review", "最后一项未执行"
	if err := server.publishExecutionTask(task, false); err != nil {
		t.Fatal(err)
	}
	updated, err := server.tasks.ReadReport(t.Context(), server.root, application.ScanOptions{}, false)
	if err != nil || updated.Metadata.ScanTaskID == previous.Metadata.ScanTaskID {
		t.Fatalf("终态没有预热新的增量快照: err=%v metadata=%+v", err, updated.Metadata)
	}
	if len(updated.Report.Conversations) != 1 || updated.Report.Conversations[0].ID != ids[2] {
		t.Fatalf("未执行的会话被误隐藏: %+v", updated.Report.Conversations)
	}
}

func TestExecutionProgressViewOmitsHeavyEvidenceWithoutChangingStoredTask(t *testing.T) {
	server, ids := executionFixture(t, 1)
	handler := server.Handler()
	confirmation := prepareExecution(t, handler, ids, false)
	response := submitExecution(t, handler, confirmation)
	var started executionTask
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &started) != nil {
		t.Fatal(response.Body.String())
	}
	finished := awaitExecution(t, handler, started.ID)
	if finished.State != "completed" {
		t.Fatalf("合成删除失败: %+v", finished)
	}
	for _, endpoint := range []string{"/api/v1/operations/executions", "/api/v1/operations/executions/" + started.ID} {
		full := apiJSON(t, handler, http.MethodGet, endpoint, "")
		progress := apiJSON(t, handler, http.MethodGet, endpoint+"?view=progress", "")
		if full.Code != http.StatusOK || progress.Code != http.StatusOK || progress.Body.Len() >= full.Body.Len() {
			t.Fatalf("轻量进度未减少传输: full=%d progress=%d", full.Body.Len(), progress.Body.Len())
		}
		decode := func(data []byte) executionTask {
			var value executionTask
			if endpoint == "/api/v1/operations/executions" {
				var list struct {
					Items []executionTask `json:"items"`
				}
				if err := json.Unmarshal(data, &list); err != nil || len(list.Items) != 1 {
					t.Fatalf("进度列表不可解析: %s", data)
				}
				return list.Items[0]
			}
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
		compact, original := decode(progress.Body.Bytes()), decode(full.Body.Bytes())
		if compact.ID != original.ID || compact.Completed != 1 || compact.State != "completed" || compact.UpdatedAt.IsZero() || compact.Strategy.PermanentDelete != original.Strategy.PermanentDelete {
			t.Fatalf("进度视图丢失执行结论: %+v", compact)
		}
		item := compact.Preview.Items[0]
		if item.ConversationID != ids[0] || !item.Completed || item.Result == nil || item.Result.JournalPath == "" || item.Plan != nil || item.AssetGraph != nil || item.Activity != nil {
			t.Fatalf("进度身份不完整或仍带大块证据: %+v", item)
		}
		if original.Preview.Items[0].Plan == nil || original.Preview.Items[0].AssetGraph == nil {
			t.Fatal("读取进度破坏了完整执行记录")
		}
	}
}

func TestCompletedRemovalInvalidatesCachedConversationList(t *testing.T) {
	server, ids := executionFixture(t, 2)
	handler := server.Handler()
	warm := apiJSON(t, handler, http.MethodGet, "/api/v1/conversations", "")
	if warm.Code != http.StatusOK {
		t.Fatal(warm.Body.String())
	}
	confirmation := prepareExecution(t, handler, ids[:1], false)
	response := submitExecution(t, handler, confirmation)
	var started executionTask
	if response.Code != http.StatusAccepted || json.Unmarshal(response.Body.Bytes(), &started) != nil {
		t.Fatal(response.Body.String())
	}
	if finished := awaitExecution(t, handler, started.ID); finished.State != "completed" {
		t.Fatalf("合成删除未完成: %+v", finished)
	}
	response = apiJSON(t, handler, http.MethodGet, "/api/v1/conversations", "")
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &list) != nil {
		t.Fatal(response.Body.String())
	}
	if len(list.Items) != 1 || list.Items[0].ID != ids[1] {
		t.Fatalf("已完成删除的会话仍被旧快照返回: %s", response.Body.String())
	}
}

func TestLiveExecutionProgressBindsCompletedTargetsAndPurgedRecovery(t *testing.T) {
	work := t.TempDir()
	task := &executionTask{RecoveryCommand: "recover", Preview: batchOperationResponse{Items: []batchOperationItem{
		{SourceID: "a", Plan: &lifecycle.OperationPlan{OperationID: "operation"}},
		{SourceID: "b", Plan: &lifecycle.OperationPlan{OperationID: "operation"}},
	}}}
	write := func(value any) {
		t.Helper()
		data, _ := json.Marshal(value)
		if err := os.WriteFile(filepath.Join(work, "journal.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"completed_ids": []string{"a"}, "current_batch": 1, "current_item": 2})
	if err := readLiveExecutionProgress(task, work); err != nil || task.Completed != 1 || task.CurrentItem != 2 || task.Preview.Items[1].Completed {
		t.Fatalf("部分进度错误：%+v %v", task, err)
	}
	write(map[string]any{"completed_ids": []string{"a", "b"}, "recovery_purged": true})
	if err := readLiveExecutionProgress(task, work); err != nil || task.Completed != 2 || !task.RecoveryPurged || task.RecoveryCommand != "" {
		t.Fatalf("永久删除后仍显示可恢复：%+v %v", task, err)
	}
	task.RecoveryCommand = "recover-from-package"
	write(map[string]any{"completed_ids": []string{"a", "b"}, "recovery_purged": true,
		"trash_package": map[string]any{"state": "published", "receipt": map[string]string{"trash_path": "/fixture-trash/对话回收包"}}})
	if err := readLiveExecutionProgress(task, work); err != nil || !task.PackagedTrash || task.TrashPackagePath != "/fixture-trash/对话回收包" || task.RecoveryCommand == "" {
		t.Fatalf("回收包被误认为不可恢复或仍指向临时备份：%+v %v", task, err)
	}
	write(map[string]any{"completed_ids": []string{"other"}})
	if err := readLiveExecutionProgress(task, work); err == nil {
		t.Fatal("计划外进度没有阻断")
	}
}
