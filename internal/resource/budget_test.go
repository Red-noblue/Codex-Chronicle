// 中文说明：验证资源预算只统计本项目目录、拒绝符号链接并在超限时 fail-closed。
package resource

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectAndCheckBudget(t *testing.T) {
	base := t.TempDir()
	persistent := filepath.Join(base, "archive")
	temporary := filepath.Join(base, "tmp")
	if err := os.MkdirAll(persistent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persistent, "archive.db"), make([]byte, 17), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary, "journal"), make([]byte, 9), 0o600); err != nil {
		t.Fatal(err)
	}
	budget := Budget{MemoryLimitBytes: DefaultMemoryLimitBytes, PersistentLimitBytes: 20, TemporaryLimitBytes: 10}
	snapshot, err := budget.Check(context.Background(), Paths{Persistent: []string{persistent}, Temporary: []string{temporary}})
	if err != nil || !snapshot.WithinBudget || snapshot.PersistentBytes != 17 || snapshot.TemporaryBytes != 9 {
		t.Fatalf("unexpected budget snapshot: %+v err=%v", snapshot, err)
	}
	budget.PersistentLimitBytes = 16
	snapshot, err = budget.Check(context.Background(), Paths{Persistent: []string{persistent}})
	if err == nil || snapshot.WithinBudget || snapshot.PersistentBytes != 17 {
		t.Fatalf("persistent overflow was not rejected: %+v err=%v", snapshot, err)
	}
}

func TestInspectSkipsSymlinkTargets(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "secret"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	snapshot := Budget{PersistentLimitBytes: 1}.Inspect(context.Background(), Paths{Persistent: []string{root}})
	if snapshot.PersistentBytes != 0 || !snapshot.WithinBudget {
		t.Fatalf("symlink target should not count: %+v", snapshot)
	}
}
