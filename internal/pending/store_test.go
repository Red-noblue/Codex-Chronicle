// 中文说明：验证暂存操作的原子持久化、重启读取、状态机和取消留痕。
package pending

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStorePersistsAcrossNewStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "pending")
	first, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := NewOperation("workspace-a", filepath.Join(t.TempDir(), "source"), "conversation-a", Strategy{Archive: true, Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	operation.State = StateWaitingForIdle
	operation.BlockReasons = []string{"文件仍被进程打开"}
	operation.LastProbeAt = time.Now().UTC()
	if err := first.Save(context.Background(), operation); err != nil {
		t.Fatal(err)
	}
	second, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := second.Get(context.Background(), operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != StateWaitingForIdle || loaded.ConversationID != operation.ConversationID || len(loaded.BlockReasons) != 1 {
		t.Fatalf("persisted operation mismatch: %+v", loaded)
	}
	items, err := second.List(context.Background())
	if err != nil || len(items) != 1 {
		t.Fatalf("list mismatch: len=%d err=%v", len(items), err)
	}
}

func TestStateTransitionAndTerminalIdempotence(t *testing.T) {
	operation := Operation{State: StatePending}
	for _, state := range []State{StateWaitingForIdle, StateIdleDetected, StateConfirmationRequired, StateReadyForFixtureExecution, StateCompleted} {
		if !operation.CanTransition(state) {
			t.Fatalf("transition %s should be allowed from %s", state, operation.State)
		}
		operation.State = state
	}
	if operation.CanTransition(StateWaitingForIdle) {
		t.Fatal("completed operation must not return to waiting")
	}
	if !operation.IsTerminal() {
		t.Fatal("completed operation should be terminal")
	}
}

func TestDefaultRootIsOutsideSourceRoot(t *testing.T) {
	source := filepath.Join(t.TempDir(), ".codex")
	root := DefaultRoot(source, "")
	if filepath.Dir(root) == source || root == source {
		t.Fatalf("pending root was placed inside source: %s", root)
	}
	insideArchive := filepath.Join(source, "archive.db")
	root = DefaultRoot(source, insideArchive)
	if root == filepath.Join(source, "pending-operations") || pathWithin(source, root) {
		t.Fatalf("archive.db 在源根内时 pending root 没有隔离: %s", root)
	}
}
