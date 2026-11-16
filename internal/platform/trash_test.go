// 中文说明：验证目录回收站的移动、恢复、冲突保护和批量失败补偿。
package platform

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-chronicle.local/internal/compat"
	"codex-chronicle.local/internal/domain"
	"codex-chronicle.local/internal/lifecycle"
)

func trashDescriptor() compat.Descriptor {
	return compat.Descriptor{ID: "fixture-trash", SupportedVersions: []string{"fixture-v1"}, Capabilities: map[compat.Capability]bool{
		compat.CapabilityRead: true, compat.CapabilityDelete: true,
	}}
}

func trashPlan(t *testing.T, root string, paths ...string) lifecycle.OperationPlan {
	t.Helper()
	assets := make([]domain.FileInventory, 0, len(paths))
	for _, path := range paths {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		assets = append(assets, domain.FileInventory{Asset: domain.Asset{RelativePath: path, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Status: domain.StatusStable}})
	}
	conversation := domain.ConversationSummary{ID: "trash-conversation", AssetSetHash: "hash", Status: domain.StatusStable}
	plan, err := lifecycle.BuildPlan(context.Background(), root, conversation, assets, trashDescriptor(), lifecycle.PlanOptions{Trash: true})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestMoveFileNoReplacePreservesExistingTarget(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	target := filepath.Join(base, "target")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveFileNoReplace(source, target); err == nil {
		t.Fatal("expected existing target to block no-replace move")
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "source" {
		t.Fatalf("source was changed after target collision: %q err=%v", data, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "target" {
		t.Fatalf("existing target was overwritten: %q err=%v", data, err)
	}
}

func TestDirectoryTrashMovesAndRestoresWithoutOverwrite(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "one.jsonl")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := trashPlan(t, root, "sessions/one.jsonl")
	trash, err := NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ExecuteTrash(context.Background(), plan, trash)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("unexpected move result: receipts=%+v err=%v", receipts, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists after move: %v", err)
	}
	if err := trash.Restore(context.Background(), receipts[0], root); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "one" {
		t.Fatalf("restored content mismatch: %q err=%v", data, err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := trash.Restore(context.Background(), receipts[0], root); err == nil {
		t.Fatal("expected restore collision")
	}
}

func TestDirectoryTrashRejectsRestoreReceiptOutsideRoot(t *testing.T) {
	root := t.TempDir()
	trashRoot := filepath.Join(t.TempDir(), "trash")
	trash, err := NewDirectoryTrash(trashRoot)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = trash.Restore(context.Background(), TrashReceipt{OperationID: "op", RelativePath: "restored.txt", TrashPath: outside}, root)
	if err == nil {
		t.Fatal("expected restore to reject a receipt outside the trash root")
	}
	if _, statErr := os.Stat(outside); statErr != nil {
		t.Fatalf("outside receipt source was changed: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "restored.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("restore created an unexpected source file: %v", statErr)
	}
}

func TestFilesystemTrashMovesRestoresAndWritesLinuxMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "sessions", "one.jsonl")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := trashPlan(t, root, "sessions/one.jsonl")
	trashRoot := filepath.Join(t.TempDir(), "Trash", "files")
	trash, err := NewFilesystemTrash(trashRoot, "linux")
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ExecuteTrash(context.Background(), plan, trash)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("unexpected filesystem trash result: receipts=%+v err=%v", receipts, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("source still exists after filesystem trash move: %v", err)
	}
	infoPath := filepath.Join(filepath.Dir(trashRoot), "info", filepath.Base(receipts[0].TrashPath)+".trashinfo")
	data, err := os.ReadFile(infoPath)
	if err != nil || len(data) == 0 {
		t.Fatalf("linux trash metadata missing: %q err=%v", data, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "[Trash Info]" || lines[1] != "Path="+(&url.URL{Path: path}).EscapedPath() || !strings.HasPrefix(lines[2], "DeletionDate=") {
		t.Fatalf("linux trash metadata does not match freedesktop format: got=%q", data)
	}
	deletedAt, err := time.ParseInLocation("2006-01-02T15:04:05", strings.TrimPrefix(lines[2], "DeletionDate="), time.Local)
	if err != nil || time.Since(deletedAt) < -time.Second || time.Since(deletedAt) > time.Minute {
		t.Fatalf("linux trash metadata date is not a recent local timestamp: got=%q parsed=%v err=%v", lines[2], deletedAt, err)
	}
	if err := trash.Restore(context.Background(), receipts[0], root); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "one" {
		t.Fatalf("restored filesystem trash content mismatch: %q err=%v", data, err)
	}
	if _, err := os.Stat(infoPath); !os.IsNotExist(err) {
		t.Fatalf("linux trash metadata remained after restore: %v", err)
	}
}

func TestFilesystemTrashMovesAndRestoresArchiveDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archives")
	archive := filepath.Join(root, "2026-11-14T10-30-00Z")
	if err := os.MkdirAll(filepath.Join(archive, "payload", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		filepath.Join(archive, "manifest.json"):                        "{\"version\":1}\n",
		filepath.Join(archive, "payload", "sessions", "rollout.jsonl"): "conversation data\n",
	} {
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trashRoot := filepath.Join(base, "Trash", "files")
	trash, err := NewFilesystemTrash(trashRoot, "linux")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := trash.Move(context.Background(), "archive-delete-test", root, filepath.Base(archive))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("归档目录仍留在原位置: %v", err)
	}
	for relative, expected := range map[string]string{
		"manifest.json": "{\"version\":1}\n",
		filepath.Join("payload", "sessions", "rollout.jsonl"): "conversation data\n",
	} {
		actual, err := os.ReadFile(filepath.Join(receipt.TrashPath, relative))
		if err != nil || string(actual) != expected {
			t.Fatalf("回收站中的归档内容不完整: path=%s value=%q err=%v", relative, actual, err)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(trashRoot), "info", filepath.Base(receipt.TrashPath)+".trashinfo")); err != nil {
		t.Fatalf("缺少 Linux 回收站恢复元数据: %v", err)
	}
	if err := trash.Restore(context.Background(), receipt, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archive, "manifest.json")); err != nil {
		t.Fatalf("归档文件夹没有恢复到原位置: %v", err)
	}
}

func TestFilesystemTrashRejectsArchiveDirectoryWithSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "archives")
	archive := filepath.Join(root, "unsafe")
	if err := os.MkdirAll(archive, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(archive, "payload-link")); err != nil {
		t.Skipf("当前环境不允许创建符号链接: %v", err)
	}
	trash, err := NewFilesystemTrash(filepath.Join(base, "Trash", "files"), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := trash.Move(context.Background(), "archive-delete-test", root, "unsafe"); err == nil {
		t.Fatal("含符号链接的归档目录不应进入回收站")
	}
	if _, err := os.Lstat(archive); err != nil {
		t.Fatalf("拒绝后源归档目录应保留: %v", err)
	}
}

func TestFilesystemTrashEscapesLinuxTrashInfoPath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "源 #目录", "项目")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a #?% 对话.jsonl")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := trashPlan(t, root, filepath.Base(path))
	trashRoot := filepath.Join(base, "Trash", "files")
	trash, err := NewFilesystemTrash(trashRoot, "linux")
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ExecuteTrash(context.Background(), plan, trash)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("unexpected filesystem trash result: receipts=%+v err=%v", receipts, err)
	}
	infoPath := filepath.Join(filepath.Dir(trashRoot), "info", filepath.Base(receipts[0].TrashPath)+".trashinfo")
	data, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Path="+(&url.URL{Path: path}).EscapedPath()+"\n") || strings.Contains(string(data), "file://") {
		t.Fatalf("trashinfo Path is not URI-escaped absolute path: %q", data)
	}
	if err := trash.Restore(context.Background(), receipts[0], root); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemTrashMetadataCollisionDoesNotMoveSource(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "source")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "one.jsonl")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	trashRoot := filepath.Join(base, "Trash", "files")
	trash, err := NewFilesystemTrash(trashRoot, "linux")
	if err != nil {
		t.Fatal(err)
	}
	operationID := "operation-collision"
	targetName := filesystemTrashName(operationID, "one.jsonl")
	infoPath := filepath.Join(trash.InfoRoot, targetName+".trashinfo")
	if err := os.WriteFile(infoPath, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := trash.Move(context.Background(), operationID, root, "one.jsonl"); err == nil {
		t.Fatal("expected an existing trashinfo entry to block the move")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "payload" {
		t.Fatalf("metadata collision changed source file: data=%q err=%v", data, err)
	}
	if data, err := os.ReadFile(infoPath); err != nil || string(data) != "existing" {
		t.Fatalf("metadata collision overwrote existing metadata: data=%q err=%v", data, err)
	}
}

func TestFilesystemTrashUsesMacOSDirectoryWithoutLinuxMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "one.jsonl")
	if err := os.WriteFile(path, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := trashPlan(t, root, "one.jsonl")
	trash, err := NewFilesystemTrash(filepath.Join(t.TempDir(), "Trash"), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := ExecuteTrash(context.Background(), plan, trash)
	if err != nil || len(receipts) != 1 {
		t.Fatalf("unexpected macOS filesystem trash result: receipts=%+v err=%v", receipts, err)
	}
	if err := trash.Restore(context.Background(), receipts[0], root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("macOS filesystem trash restore failed: %v", err)
	}
}

type failingTrash struct {
	*DirectoryTrash
	failAfter int
	moves     int
}

func (f *failingTrash) Move(ctx context.Context, operationID, sourceRoot, relativePath string) (TrashReceipt, error) {
	if f.moves >= f.failAfter {
		return TrashReceipt{}, os.ErrPermission
	}
	f.moves++
	return f.DirectoryTrash.Move(ctx, operationID, sourceRoot, relativePath)
}

func TestExecuteTrashRollsBackEarlierMovesOnFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.jsonl"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.jsonl"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan := trashPlan(t, root, "a.jsonl", "b.jsonl")
	directory, err := NewDirectoryTrash(filepath.Join(t.TempDir(), "trash"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &failingTrash{DirectoryTrash: directory, failAfter: 1}
	if _, err := ExecuteTrash(context.Background(), plan, provider); err == nil {
		t.Fatal("expected injected move failure")
	}
	for _, path := range []string{"a.jsonl", "b.jsonl"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("rollback did not restore %s: %v", path, err)
		}
	}
}
