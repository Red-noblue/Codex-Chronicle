//go:build darwin && cgo

// 中文说明：验证 Foundation 原生回收站能参与 journal saga 隔离与回滚。
package operation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/lifecycle"
	"codex-chronicle.local/internal/platform"
)

func TestNativeTrashSagaIsolationAndRecovery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rollout.jsonl")
	content := []byte("native trash saga fixture\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	plan := sagaPlan(t, root, true)
	trash, err := platform.NewNativeSystemTrash()
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteLocalSaga(context.Background(), plan, filepath.Join(t.TempDir(), "archives"), filepath.Join(t.TempDir(), "journal"), trash)
	if err == nil || result.Phase != lifecycle.PhaseNeedsReview || len(result.TrashReceipts) != 1 {
		t.Fatalf("expected native-trash saga to stop for review: result=%+v err=%v", result, err)
	}
	t.Cleanup(func() {
		if len(result.TrashReceipts) == 1 {
			if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
				if restoreErr := trash.Restore(context.Background(), result.TrashReceipts[0], root); restoreErr != nil {
					t.Errorf("restore fixture during cleanup: %v", restoreErr)
				}
			}
		}
	})
	recovered, recoverErr := RecoverLocalSaga(context.Background(), result.JournalPath, trash)
	if recoverErr != nil || recovered.Phase != lifecycle.PhaseRolledBack {
		t.Fatalf("native-trash saga recovery failed: result=%+v err=%v", recovered, recoverErr)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(content) {
		t.Fatalf("native-trash saga did not restore source: data=%q err=%v", data, err)
	}
}
