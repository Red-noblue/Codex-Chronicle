// 中文说明：验证副本后台任务不会把完整文件清单塞进 API 快照，并支持计划确认、取消和验证。
package copy

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/activity"
	"codex-chronicle.local/internal/domain"
)

func TestManagerPrepareExecuteAndVerify(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	destination := filepath.Join(base, "nested", "copy")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), `{"type":"session_meta"}`+"\n")
	manager := NewManager()
	initial := manager.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	if initial.Status != domain.TaskQueued || initial.Kind != "copy_prepare" {
		t.Fatalf("unexpected initial task: %+v", initial)
	}
	prepared := waitCopyTask(t, manager, initial.ID)
	if prepared.Status != domain.TaskCompleted || prepared.Plan == nil || prepared.Plan.FileCount != 1 {
		t.Fatalf("prepare task failed: %+v", prepared)
	}
	if len(prepared.Plan.Warnings) != 0 {
		t.Fatalf("unexpected plan warnings: %+v", prepared.Plan)
	}

	executing, err := manager.StartExecute(initial.ID, ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitCopyTask(t, manager, executing.ID)
	if completed.Status != domain.TaskCompleted || completed.Result == nil || !completed.Result.Published {
		t.Fatalf("execute task failed: %+v", completed)
	}
	verified := manager.StartVerify(destination)
	verifiedSnapshot := waitCopyTask(t, manager, verified.ID)
	if verifiedSnapshot.Status != domain.TaskCompleted || verifiedSnapshot.Result == nil {
		t.Fatalf("verify task failed: %+v", verifiedSnapshot)
	}
}

func TestManagerPersistsPlanOutsideTaskMemory(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	planRoot := filepath.Join(base, "derived", "copy-plans")
	destination := filepath.Join(base, "copy")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), `{"type":"session_meta"}`+"\n")
	manager := NewManagerWithPlanRoot(planRoot)
	initial := manager.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	prepared := waitCopyTask(t, manager, initial.ID)
	if prepared.Status != domain.TaskCompleted || prepared.Plan == nil {
		t.Fatalf("persisted plan preparation failed: %+v", prepared)
	}
	manager.mu.RLock()
	state := manager.tasks[initial.ID]
	planPath := state.planPath
	planInMemory := state.plan
	manager.mu.RUnlock()
	if planPath == "" || len(planInMemory.Files) != 0 {
		t.Fatalf("完整计划仍保留在任务内存：path=%q files=%d", planPath, len(planInMemory.Files))
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("持久化计划不存在：%v", err)
	}
	if _, err := os.Stat(planPath + publishedPlanSuffix); !os.IsNotExist(err) {
		t.Fatalf("未发布计划不应存在已发布标记：%v", err)
	}
	executing, err := manager.StartExecute(initial.ID, ExecuteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	completed := waitCopyTask(t, manager, executing.ID)
	if completed.Status != domain.TaskCompleted || completed.Result == nil || !completed.Result.Published {
		t.Fatalf("从持久化计划执行失败：%+v", completed)
	}
}

func TestManagerRestoresPersistedPlanAfterRestart(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	planRoot := filepath.Join(base, "derived", "copy-plans")
	destination := filepath.Join(base, "copy-after-restart")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), `{"type":"session_meta"}`+"\n")
	first := NewManagerWithPlanRoot(planRoot)
	initial := first.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	prepared := waitCopyTask(t, first, initial.ID)
	if prepared.Status != domain.TaskCompleted || prepared.Plan == nil {
		t.Fatalf("首次 prepare 失败：%+v", prepared)
	}

	second := NewManagerWithPlanRoot(planRoot)
	restored, ok := second.Get(initial.ID)
	if !ok || restored.Status != domain.TaskCompleted || restored.Plan == nil || restored.Plan.SourceManifestSHA256 != prepared.Plan.SourceManifestSHA256 {
		t.Fatalf("重启后没有恢复同一副本计划：ok=%v restored=%+v prepared=%+v", ok, restored, prepared)
	}
	executing, err := second.StartExecute(initial.ID, ExecuteOptions{})
	if err != nil {
		t.Fatalf("重启后执行持久化计划失败：%v", err)
	}
	completed := waitCopyTask(t, second, executing.ID)
	if completed.Status != domain.TaskCompleted || completed.Result == nil || !completed.Result.Published {
		t.Fatalf("重启后副本执行失败：%+v", completed)
	}
	if _, err := VerifyDestination(context.Background(), destination); err != nil {
		t.Fatalf("重启后副本验证失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(planRoot, "copy-plan-"+initial.ID+publishedPlanSuffix)); err != nil {
		t.Fatalf("发布后状态标记不存在：%v", err)
	}
	if listed := second.ListPrepared(); len(listed) != 0 {
		t.Fatalf("已发布计划不应继续出现在待确认列表：%+v", listed)
	}
	third := NewManagerWithPlanRoot(planRoot)
	if listed := third.ListPrepared(); len(listed) != 0 {
		t.Fatalf("重启后已发布计划不应再次出现：%+v", listed)
	}
	if _, err := third.StartExecute(initial.ID, ExecuteOptions{}); err == nil {
		t.Fatal("已发布计划不应允许重复执行")
	}
}

func TestManagerAllowsOnlyOneConcurrentExecuteForPreparedPlan(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	planRoot := filepath.Join(base, "derived", "copy-plans")
	destination := filepath.Join(base, "copy")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload\n")
	manager := NewManagerWithPlanRoot(planRoot)
	prepared := manager.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: destination, ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	waitCopyTask(t, manager, prepared.ID)
	first, firstErr := manager.StartExecute(prepared.ID, ExecuteOptions{})
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	if _, secondErr := manager.StartExecute(prepared.ID, ExecuteOptions{}); secondErr == nil {
		t.Fatal("同一副本计划不应允许并发执行")
	}
	completed := waitCopyTask(t, manager, first.ID)
	if completed.Status != domain.TaskCompleted || completed.Result == nil || !completed.Result.Published {
		t.Fatalf("唯一执行任务未完成：%+v", completed)
	}
}

func TestManagerListsPersistedPreparedPlansWithoutFullFileList(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	planRoot := filepath.Join(base, "derived", "copy-plans")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload\n")
	manager := NewManagerWithPlanRoot(planRoot)
	initial := manager.StartPrepare(CopyRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	waitCopyTask(t, manager, initial.ID)
	listed := NewManagerWithPlanRoot(planRoot).ListPrepared()
	if len(listed) != 1 || listed[0].ID != initial.ID || listed[0].Plan == nil {
		t.Fatalf("落盘计划摘要未被列出：%+v", listed)
	}
	if listed[0].Plan.FileCount != 1 || listed[0].Plan.SourceManifestSHA256 == "" {
		t.Fatalf("落盘计划摘要不完整：%+v", listed[0].Plan)
	}
	managerState := NewManagerWithPlanRoot(planRoot)
	managerState.ListPrepared()
	managerState.mu.RLock()
	state := managerState.tasks[initial.ID]
	managerState.mu.RUnlock()
	if state == nil || len(state.plan.Files) != 0 {
		t.Fatalf("列出计划时不应把完整文件清单载入任务内存：%+v", state)
	}
}

func TestManagerBindsPlansToWorkspaceAtTaskCreation(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	planRootA := filepath.Join(base, "workspace-a", "plans")
	planRootB := filepath.Join(base, "workspace-b", "plans")
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload\n")
	manager := NewManagerWithPlanRoot(planRootA)
	first := manager.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: filepath.Join(base, "copy-a"), ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	manager.SetPlanRoot(planRootB)
	second := manager.StartPrepare(CopyRequest{AllowUnavailableProcessProbe: runtime.GOOS == "windows", SourceRoot: source, DestinationRoot: filepath.Join(base, "copy-b"), ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	waitCopyTask(t, manager, first.ID)
	waitCopyTask(t, manager, second.ID)
	if _, err := os.Stat(filepath.Join(planRootA, "copy-plan-"+first.ID+".json")); err != nil {
		t.Fatalf("工作区 A 计划未写入原目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(planRootB, "copy-plan-"+second.ID+".json")); err != nil {
		t.Fatalf("工作区 B 计划未写入新目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(planRootA, "copy-plan-"+second.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("工作区 B 计划错误写入 A 目录：%v", err)
	}
	listed := manager.ListPrepared()
	if len(listed) != 1 || listed[0].ID != second.ID {
		t.Fatalf("切换到工作区 B 后不应列出 A 的计划：%+v", listed)
	}
	if _, err := manager.StartExecute(first.ID, ExecuteOptions{}); err == nil || !strings.Contains(err.Error(), "其他工作区") {
		t.Fatalf("切换工作区后不应执行 A 的计划：%v", err)
	}
}

func TestManagerCancelPrepare(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	for index := 0; index < 20; index++ {
		writeCopyTestFile(t, filepath.Join(source, "sessions", "file-"+string(rune('a'+index))+".jsonl"), "payload")
	}
	manager := NewManager()
	initial := manager.StartPrepare(CopyRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ChunkBytes: 32 << 10, ActivityOptions: &activity.Options{Interval: 5 * time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	if !manager.Cancel(initial.ID) {
		t.Fatal("cancel should find queued task")
	}
	result := waitCopyTask(t, manager, initial.ID)
	if result.Status != domain.TaskCancelled && result.Status != domain.TaskFailed {
		t.Fatalf("unexpected cancelled task status: %+v", result)
	}
}

func TestManagerFailedPrepareRetainsActivitySummary(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, ".codex")
	if err := os.MkdirAll(filepath.Join(source, "thread-writer-locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCopyTestFile(t, filepath.Join(source, "rollout.jsonl"), "payload")
	lockID := "33333333-3333-3333-3333-333333333333"
	writeCopyTestFile(t, filepath.Join(source, "thread-writer-locks", lockID+".lock"), "lock")
	manager := NewManager()
	initial := manager.StartPrepare(CopyRequest{SourceRoot: source, DestinationRoot: filepath.Join(base, "copy"), ActivityOptions: &activity.Options{Interval: time.Millisecond, ProcessDetector: stableProcessDetector{}}})
	failed := waitCopyTask(t, manager, initial.ID)
	if failed.Status != domain.TaskFailed || failed.Plan == nil {
		t.Fatalf("failed prepare should retain a summary: %+v", failed)
	}
	if len(failed.Plan.Activity.LockIDs) != 1 || failed.Plan.Activity.LockIDs[0] != lockID {
		t.Fatalf("failed prepare activity summary missing lock ID: %+v", failed.Plan.Activity)
	}
	if len(failed.Plan.Activity.Results) == 0 || len(failed.Plan.Activity.Results) > 32 {
		t.Fatalf("failed prepare should retain only bounded activity samples: %+v", failed.Plan.Activity.Results)
	}
}

func waitCopyTask(t *testing.T, manager *Manager, id string) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, ok := manager.Get(id)
		if ok && (snapshot.Status == domain.TaskCompleted || snapshot.Status == domain.TaskFailed || snapshot.Status == domain.TaskCancelled) {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task did not finish: %s", id)
	return TaskSnapshot{}
}
