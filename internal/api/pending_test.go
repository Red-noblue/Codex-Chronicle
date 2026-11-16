// 中文说明：覆盖活动期间暂存、空闲复核、重启恢复、重新确认和重复确认幂等性。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/pending"
)

type pendingToggleProcessDetector struct {
	open bool
}

func (detector *pendingToggleProcessDetector) Detect(_ context.Context, root string) activity.ProcessSnapshot {
	if !detector.open {
		return activity.ProcessSnapshot{Available: true, Reason: "测试中没有打开进程", Files: map[string][]domain.ProcessHandle{}}
	}
	return activity.ProcessSnapshot{Available: true, Reason: "测试进程打开 rollout", Files: map[string][]domain.ProcessHandle{filepath.Join(root, "rollout.jsonl"): {{PID: 701, Command: "codex-test"}}}}
}

func pendingJSON(t *testing.T, handler http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	handler.ServeHTTP(recorder, request)
	parsed := make(map[string]any)
	if recorder.Body.Len() != 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
			t.Fatalf("响应不是 JSON: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
	}
	return recorder.Code, parsed
}

func TestPendingOperationWaitsForIdleAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "rollout.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"pending-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detector := &pendingToggleProcessDetector{open: true}
	options := &LifecycleOptions{
		Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(),
		ActivityOptions: &activity.Options{Interval: time.Millisecond, Conservative: false, ProcessDetector: detector},
	}
	server := NewServerWithOptions(root, nil, options)
	handler := server.Handler()
	conversationID := compat.StableConversationID("pending-thread")
	status, body := pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending", `{"conversation_id":"`+conversationID+`","archive":true,"trash":true}`)
	if status != http.StatusAccepted {
		t.Fatalf("创建暂存操作失败: status=%d body=%v", status, body)
	}
	operation := decodePendingOperation(t, body)
	if operation.State != pending.StateWaitingForIdle || len(operation.BlockReasons) == 0 {
		t.Fatalf("活动期间没有进入等待空闲: %+v", operation)
	}

	detector.open = false
	status, body = pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/refresh", "{}")
	if status != http.StatusOK {
		t.Fatalf("空闲刷新失败: status=%d body=%v", status, body)
	}
	operation = decodePendingOperation(t, body)
	if operation.State != pending.StateIdleDetected {
		t.Fatalf("空闲刷新没有保留 idle_detected: %+v", operation)
	}

	status, body = pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/preview", `{"archive":true,"trash":true}`)
	if status != http.StatusOK {
		t.Fatalf("重新生成暂存预览失败: status=%d body=%v", status, body)
	}
	operation = decodePendingOperation(t, body)
	if operation.State != pending.StateConfirmationRequired || operation.Preview == nil {
		t.Fatalf("暂存预览没有进入人工确认: %+v", operation)
	}

	// 新建 daemon 只读取同一个源根外部的 pending 目录，验证突然重启后意图没有丢失。
	restarted := NewServerWithOptions(root, nil, options).Handler()
	status, body = pendingJSON(t, restarted, http.MethodGet, "/api/v1/operations/pending", "")
	if status != http.StatusOK {
		t.Fatalf("重启后读取 pending 失败: status=%d body=%v", status, body)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("重启后 pending 数量错误: %v", body["items"])
	}

	status, body = pendingJSON(t, restarted, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/confirm", `{"confirmation_code":""}`)
	if status != http.StatusOK {
		t.Fatalf("首次重新确认失败: status=%d body=%v", status, body)
	}
	operation = decodePendingOperation(t, body)
	if operation.State != pending.StateReadyForFixtureExecution || operation.LastReconfirmedAt.IsZero() {
		t.Fatalf("重新确认没有进入 fixture 执行准备态: %+v", operation)
	}
	lastReconfirmed := operation.LastReconfirmedAt
	status, body = pendingJSON(t, restarted, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/confirm", `{"confirmation_code":""}`)
	if status != http.StatusOK || body["idempotent"] != true {
		t.Fatalf("重复确认没有幂等返回: status=%d body=%v", status, body)
	}
	operation = decodePendingOperation(t, body)
	if !operation.LastReconfirmedAt.Equal(lastReconfirmed) {
		t.Fatalf("重复确认改变了重新确认时间: old=%s new=%s", lastReconfirmed, operation.LastReconfirmedAt)
	}
}

func TestPendingOperationReturnsToWaitingWhenConversationResumes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"pending-resume\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detector := &pendingToggleProcessDetector{}
	options := &LifecycleOptions{Enabled: true, FixtureOnly: true, Descriptor: compat.SyntheticFixtureDescriptor(), ActivityOptions: &activity.Options{Interval: time.Millisecond, Conservative: false, ProcessDetector: detector}}
	handler := NewServerWithOptions(root, nil, options).Handler()
	conversationID := compat.StableConversationID("pending-resume")
	status, body := pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending", `{"conversation_id":"`+conversationID+`","archive":true}`)
	if status != http.StatusAccepted {
		t.Fatalf("创建空闲 pending 失败: status=%d body=%v", status, body)
	}
	operation := decodePendingOperation(t, body)
	if operation.State != pending.StateConfirmationRequired {
		t.Fatalf("初始空闲 pending 状态错误: %+v", operation)
	}
	detector.open = true
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"pending-resume\"}}\n{\"type\":\"response_item\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	status, body = pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/refresh", "{}")
	if status != http.StatusConflict {
		t.Fatalf("会话恢复活动时没有阻断刷新: status=%d body=%v", status, body)
	}
	operation = decodePendingOperation(t, body)
	if operation.State != pending.StateRevalidationFailed && operation.State != pending.StateWaitingForIdle {
		t.Fatalf("会话恢复活动后的状态错误: %+v", operation)
	}
}

func TestPendingRepairIntentIsSavedWithoutWritingSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".codex")
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"pending-repair\"}}\n")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	options := &LifecycleOptions{ActivityOptions: &activity.Options{Interval: time.Millisecond, Conservative: false, DisableProcessProbe: true}}
	handler := NewServerWithOptions(root, nil, options).Handler()
	conversationID := compat.StableConversationID("pending-repair")
	status, body := pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending", `{"conversation_id":"`+conversationID+`","kind":"repair"}`)
	if status != http.StatusAccepted {
		t.Fatalf("修复意图没有被持久化: status=%d body=%v", status, body)
	}
	operation := decodePendingOperation(t, body)
	if operation.Kind != "repair" || operation.SourceRoot != root {
		t.Fatalf("修复意图字段错误: %+v", operation)
	}
	status, body = pendingJSON(t, handler, http.MethodPost, "/api/v1/operations/pending/"+operation.ID+"/preview", `{"kind":"repair"}`)
	if status != http.StatusConflict {
		t.Fatalf("未接入的修复 adapter 没有进入人工复核: status=%d body=%v", status, body)
	}
	if actual, err := os.ReadFile(path); err != nil || string(actual) != string(content) {
		t.Fatalf("真实 .codex fixture 在暂存/预览期间被修改: err=%v content=%q", err, actual)
	}
}

func decodePendingOperation(t *testing.T, body map[string]any) pending.Operation {
	t.Helper()
	data, ok := body["operation"]
	if !ok {
		t.Fatalf("响应缺少 operation: %v", body)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var operation pending.Operation
	if err := json.Unmarshal(encoded, &operation); err != nil {
		t.Fatal(err)
	}
	return operation
}
