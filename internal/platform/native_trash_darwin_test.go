//go:build darwin && cgo

// 中文说明：只用测试临时目录验证 macOS 原生回收站移动、恢复和冲突保护。
package platform

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinNativeTrashMoveRestoreAndConflict(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "会话 文件.jsonl")
	content := []byte("{\"type\":\"fixture\"}\n")
	conflict := []byte("new file must remain\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := newNativeSystemTrash()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := provider.Move(context.Background(), "darwin-trash-test", root, filepath.Base(source))
	if err != nil {
		t.Fatalf("move to native Trash: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		if data, readErr := os.ReadFile(source); readErr == nil && bytes.Equal(data, conflict) {
			if removeErr := os.Remove(source); removeErr != nil {
				t.Errorf("remove test conflict: %v", removeErr)
				return
			}
		}
		if _, statErr := os.Lstat(source); os.IsNotExist(statErr) {
			if restoreErr := provider.Restore(context.Background(), receipt, root); restoreErr != nil {
				t.Errorf("restore fixture during cleanup: %v", restoreErr)
			}
		}
	})
	trashed, err := os.ReadFile(receipt.TrashPath)
	if err != nil || !bytes.Equal(trashed, content) {
		t.Fatalf("native Trash content mismatch: data=%q err=%v", trashed, err)
	}
	if err := os.WriteFile(source, conflict, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(context.Background(), receipt, root); err == nil {
		t.Fatal("restore should reject an existing destination")
	}
	current, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(current, conflict) {
		t.Fatalf("restore changed the conflicting file: data=%q err=%v", current, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(context.Background(), receipt, root); err != nil {
		t.Fatalf("restore from native Trash: %v", err)
	}
	current, err = os.ReadFile(source)
	if err != nil || !bytes.Equal(current, content) {
		t.Fatalf("restored fixture mismatch: data=%q err=%v", current, err)
	}
	restored = true
}

func TestDarwinNativeTrashRestoreRejectsPathOutsideTrash(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.jsonl")
	content := []byte("must remain untouched\n")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := newNativeSystemTrash()
	if err != nil {
		t.Fatal(err)
	}
	receipt := TrashReceipt{OperationID: "darwin-trash-test", RelativePath: "restored.jsonl", TrashPath: outside}
	if err := provider.Restore(context.Background(), receipt, root); err == nil {
		t.Fatal("restore should reject a path outside the native Trash")
	}
	data, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("outside file changed: data=%q err=%v", data, err)
	}
}

func TestDarwinNativeTrashDirectoryMoveRestoreAndConflict(t *testing.T) {
	root := t.TempDir()
	name := "Codex-Chronicle-测试回收包"
	source := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(source, "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("仅含模拟对话数据\n")
	if err := os.WriteFile(filepath.Join(source, "files", "rollout.jsonl"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := newNativeSystemTrash()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := provider.Move(context.Background(), "directory-fixture", root, name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(receipt.TrashPath) }) // 仅清理本测试创建的模拟包。
	data, err := os.ReadFile(filepath.Join(receipt.TrashPath, "files", "rollout.jsonl"))
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("目录回收内容不完整: %v", err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(context.Background(), receipt, root); err == nil {
		t.Fatal("目录恢复不能覆盖已有目录")
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := provider.Restore(context.Background(), receipt, root); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(filepath.Join(source, "files", "rollout.jsonl"))
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("目录恢复内容不完整: %v", err)
	}
}
