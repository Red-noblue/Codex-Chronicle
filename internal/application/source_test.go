// 中文说明：验证只读源路径解析不会越出固定根目录或跟随符号链接。
package application

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveReadOnlyPath(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "sessions", "one.jsonl")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveReadOnlyPath(root, "sessions/one.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != inside {
		t.Fatalf("unexpected resolved path: %s", resolved)
	}
	for _, relative := range []string{"../outside", "/tmp/outside", "sessions/../.."} {
		if _, err := ResolveReadOnlyPath(root, relative); err == nil {
			t.Fatalf("expected path rejection for %q", relative)
		}
	}
}

func TestResolveReadOnlyPathRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.jsonl")
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := ResolveReadOnlyPath(root, "link.jsonl"); err == nil {
		t.Fatal("expected symlink rejection")
	}
}
