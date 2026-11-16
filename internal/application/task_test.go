// 中文说明：验证后台 inventory 任务的完成、进度和取消状态。
package application

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"codex-chronicle.local/internal/domain"
)

func waitTask(t *testing.T, manager *TaskManager, id string) TaskSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, ok := manager.Get(id)
		if !ok {
			t.Fatal("task disappeared")
		}
		if snapshot.Status == domain.TaskCompleted || snapshot.Status == domain.TaskFailed || snapshot.Status == domain.TaskCancelled {
			return snapshot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("task did not finish")
	return TaskSnapshot{}
}

func TestTaskManagerCompletesInventoryAndReportsProgress(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"task-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewTaskManager(NewScanner())
	initial := manager.StartInventory(root)
	if initial.Status != domain.TaskQueued || initial.ID == "" {
		t.Fatalf("unexpected initial task: %+v", initial)
	}
	completed := waitTask(t, manager, initial.ID)
	if completed.Status != domain.TaskCompleted || completed.FilesSeen != 1 || completed.FilesParsed != 1 || completed.CacheHits != 0 || completed.Report == nil || !completed.Report.ReadOnly {
		t.Fatalf("unexpected completed task: %+v", completed)
	}
}

func TestTaskManagerCancel(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 100; index++ {
		path := filepath.Join(root, filepath.Base(filepath.Join("file", string(rune('a'+index%26)))+"-"+string(rune('0'+index%10))+".jsonl"))
		if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"cancel"+string(rune('a'+index%26))+"\"}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewTaskManager(NewScanner())
	task := manager.StartInventory(root)
	if !manager.Cancel(task.ID) {
		t.Fatal("expected cancel request")
	}
	finished := waitTask(t, manager, task.ID)
	if finished.Status != domain.TaskCancelled && finished.Status != domain.TaskCompleted {
		t.Fatalf("unexpected cancellation status: %+v", finished)
	}
}

func TestTaskManagerMissingRootFails(t *testing.T) {
	manager := NewTaskManager(NewScanner())
	task := manager.StartInventory(filepath.Join(t.TempDir(), "missing"))
	finished := waitTask(t, manager, task.ID)
	if finished.Status != domain.TaskFailed || finished.Error == "" {
		t.Fatalf("unexpected failed task: %+v", finished)
	}
	if _, ok := manager.Get("missing"); ok {
		t.Fatal("unexpected missing task")
	}
}

func TestTaskManagerReusesLatestSnapshotAndDetectsSourceChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"snapshot-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewTaskManager(NewScanner())
	first, err := manager.ReadReport(t.Context(), root, ScanOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.ReadReport(t.Context(), root, ScanOptions{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.Metadata.Status != "fresh" || second.Metadata.Status != "fresh" || first.Metadata.ScanTaskID == "" || first.Metadata.ScanTaskID != second.Metadata.ScanTaskID {
		t.Fatalf("快照没有复用: first=%+v second=%+v", first.Metadata, second.Metadata)
	}
	if err := os.WriteFile(filepath.Join(root, "two.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"new-thread\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, found, err := manager.Snapshot(t.Context(), root, true)
	if err != nil || !found || !changed.Metadata.Stale || changed.Metadata.Status != "stale" {
		t.Fatalf("源文件变化没有标记 stale: found=%v err=%v metadata=%+v", found, err, changed.Metadata)
	}
}

func TestTaskManagerKeepsRootsIsolated(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootA, "a.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"root-a\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootB, "b.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"root-b\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewTaskManager(NewScanner())
	snapshotA, err := manager.ReadReport(t.Context(), rootA, ScanOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	snapshotB, err := manager.ReadReport(t.Context(), rootB, ScanOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotA.Report.Root == snapshotB.Report.Root || snapshotA.Metadata.ScanTaskID == snapshotB.Metadata.ScanTaskID || len(snapshotA.Report.Files) != 1 || snapshotA.Report.Files[0].RelativePath != "a.jsonl" || snapshotB.Report.Files[0].RelativePath != "b.jsonl" {
		t.Fatalf("工作区快照串线: A=%+v B=%+v", snapshotA, snapshotB)
	}
}

func TestTaskManagerInvalidationCancelsOldScanAndKeepsOtherRoot(t *testing.T) {
	rootA, rootB := t.TempDir(), t.TempDir()
	for _, root := range []string{rootA, rootB} {
		if err := os.WriteFile(filepath.Join(root, "one.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"thread-one\"}}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewTaskManager(NewScanner())
	if _, err := manager.ReadReport(t.Context(), rootA, ScanOptions{}, false); err != nil {
		t.Fatal(err)
	}
	other, err := manager.ReadReport(t.Context(), rootB, ScanOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	var old, fresh TaskSnapshot
	func() {
		// 阻塞文件解析，让失效前后的两个扫描都在尚未完成时经过取消边界。
		manager.scanner.state.mu.Lock()
		defer manager.scanner.state.mu.Unlock()
		old = manager.StartInventoryWithOptions(rootA, ScanOptions{})
		manager.InvalidateInventory(filepath.Join(rootA, "."))
		if _, found, err := manager.Snapshot(t.Context(), rootA, false); err != nil || found {
			t.Fatalf("失效后仍返回旧快照: found=%v err=%v", found, err)
		}
		fresh = manager.StartInventoryWithOptions(rootA, ScanOptions{})
	}()
	if old.ID == fresh.ID || fresh.Reused {
		t.Fatal("新扫描复用了失效前的任务")
	}
	if cancelled := waitTask(t, manager, old.ID); cancelled.Status != domain.TaskCancelled {
		t.Fatalf("旧扫描未被取消: %+v", cancelled)
	}
	if completed := waitTask(t, manager, fresh.ID); completed.Status != domain.TaskCompleted {
		t.Fatalf("取消旧扫描影响了新扫描: %+v", completed)
	}
	current, found, err := manager.Snapshot(t.Context(), rootA, false)
	if err != nil || !found || current.Metadata.ScanTaskID != fresh.ID {
		t.Fatalf("旧扫描覆盖了新快照: found=%v err=%v metadata=%+v", found, err, current.Metadata)
	}
	unchanged, found, err := manager.Snapshot(t.Context(), rootB, false)
	if err != nil || !found || unchanged.Metadata.ScanTaskID != other.Metadata.ScanTaskID {
		t.Fatalf("失效误伤了其他工作区: found=%v err=%v metadata=%+v", found, err, unchanged.Metadata)
	}
}

func TestTaskManagerCoalescesConcurrentReads(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < 800; index++ {
		path := filepath.Join(root, "rollout-"+strconv.Itoa(index)+".jsonl")
		if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"coalesce-"+strconv.Itoa(index)+"\"}}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewTaskManager(NewScanner())
	start := make(chan struct{})
	ids := make(chan string, 8)
	errs := make(chan error, 8)
	for index := 0; index < 8; index++ {
		go func() {
			<-start
			snapshot, err := manager.ReadReport(t.Context(), root, ScanOptions{}, false)
			if err != nil {
				errs <- err
				return
			}
			ids <- snapshot.Metadata.ScanTaskID
		}()
	}
	close(start)
	seen := make(map[string]struct{})
	for index := 0; index < 8; index++ {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-ids:
			seen[id] = struct{}{}
		}
	}
	if len(seen) != 1 {
		t.Fatalf("并发读取启动了多个扫描任务: %v", seen)
	}
}

func TestTaskManagerFailedRefreshKeepsPreviousSnapshot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stable.jsonl"), []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"stable-before-failure\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewTaskManager(NewScanner())
	previous, err := manager.ReadReport(t.Context(), root, ScanOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "stable.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	task := manager.StartInventory(root)
	finished := waitTask(t, manager, task.ID)
	if finished.Status != domain.TaskFailed {
		t.Fatalf("缺失根目录刷新没有失败: %+v", finished)
	}
	snapshot, found, snapshotErr := manager.Snapshot(t.Context(), root, false)
	if snapshotErr != nil || !found || snapshot.Metadata.ScanTaskID != previous.Metadata.ScanTaskID || len(snapshot.Report.Files) != 1 {
		t.Fatalf("失败刷新污染了上一份快照: found=%v err=%v snapshot=%+v", found, snapshotErr, snapshot)
	}
}
