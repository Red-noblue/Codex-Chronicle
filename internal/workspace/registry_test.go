// 中文说明：验证工作区注册表的路径隔离、原子持久化和独立选择语义。
package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryPersistsAndSelectsIndependentRoots(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "codex-a")
	rootB := filepath.Join(base, "codex-b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(filepath.Join(base, "config", "workspaces.json"))
	if err != nil {
		t.Fatal(err)
	}
	registry, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, err := Add(context.Background(), &registry, Definition{Name: "A", Root: rootA, ArchiveDB: filepath.Join(base, "archive-a.db")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Add(context.Background(), &registry, Definition{Name: "B", Root: rootB, ArchiveDB: filepath.Join(base, "archive-b.db")})
	if err != nil {
		t.Fatal(err)
	}
	if registry.CurrentID != a.ID {
		t.Fatalf("first workspace should become current: %+v", registry)
	}
	if _, err := Select(context.Background(), &registry, b.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), registry); err != nil {
		t.Fatal(err)
	}
	reloaded, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CurrentID != b.ID || len(reloaded.Workspaces) != 2 {
		t.Fatalf("unexpected persisted registry: %+v", reloaded)
	}
	if _, err := Remove(context.Background(), &reloaded, b.ID); err == nil {
		t.Fatal("current workspace removal should be rejected")
	}
	if _, err := Remove(context.Background(), &reloaded, a.ID); err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Workspaces) != 1 || reloaded.Workspaces[0].ID != b.ID {
		t.Fatalf("unexpected removal result: %+v", reloaded)
	}
}

func TestRegistryRejectsCodexAndOverlappingArchivePaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, ".codex")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(filepath.Join(root, "workspaces.json")); err == nil || !strings.Contains(err.Error(), ".codex") {
		t.Fatalf("expected registry .codex rejection: %v", err)
	}
	if _, err := NormalizeDefinition(Definition{Root: root, ArchiveDB: filepath.Join(root, "archive.db")}); err == nil {
		t.Fatal("expected archive.db overlap rejection")
	}
}

func TestRegistryDoesNotFollowSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := NormalizeDefinition(Definition{Root: link}); err == nil {
		t.Fatal("expected symlink root rejection")
	}
}
