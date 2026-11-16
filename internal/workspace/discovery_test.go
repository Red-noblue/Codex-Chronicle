// 中文说明：在隔离目录验证 Codex 入口识别、来源去重和无写入语义，不访问本机真实 Codex 数据。
package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func discoveryHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	return home
}

func TestDiscoverCodexHomesPrioritizesEnvironmentWithoutReadingOrWritingContents(t *testing.T) {
	home := discoveryHome(t)
	portable := filepath.Join(home, "portable")
	defaultRoot := filepath.Join(home, ".codex")
	for _, root := range []string{portable, defaultRoot} {
		if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "auth.json"), []byte("must not be parsed"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_HOME", portable)
	items, err := DiscoverCodexHomes(t.Context())
	if err != nil || len(items) != 2 || items[0].Root != portable || items[1].Root != defaultRoot {
		t.Fatalf("environment priority lost: %+v err=%v", items, err)
	}
	for _, item := range items {
		if item.Status != "available" || len(item.Markers) != 1 || item.Markers[0] != "sessions" {
			t.Fatalf("unexpected candidate: %+v", item)
		}
		entries, err := os.ReadDir(item.Root)
		if err != nil || len(entries) != 2 {
			t.Fatalf("discovery changed source: %v err=%v", entries, err)
		}
	}
	t.Setenv("CODEX_HOME", defaultRoot+string(filepath.Separator))
	items, err = DiscoverCodexHomes(t.Context())
	if err != nil || len(items) != 1 || len(items[0].Sources) != 2 {
		t.Fatalf("duplicate roots were not combined: %+v err=%v", items, err)
	}
}

func TestDiscoverCodexHomesDistinguishesMissingEmptyAndUnrecognized(t *testing.T) {
	home := discoveryHome(t)
	items, err := DiscoverCodexHomes(t.Context())
	if err != nil || len(items) != 1 || items[0].Status != "missing" {
		t.Fatalf("missing root was not reported: %+v err=%v", items, err)
	}
	root := filepath.Join(home, ".codex")
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("discovery created the missing directory")
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	items, err = DiscoverCodexHomes(t.Context())
	if err != nil || items[0].Status != "empty" {
		t.Fatalf("empty directory was not distinguished: %+v err=%v", items, err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("ordinary folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	items, err = DiscoverCodexHomes(t.Context())
	if err != nil || items[0].Status != "unrecognized" {
		t.Fatalf("ordinary folder was incorrectly recognized: %+v err=%v", items, err)
	}
}

func TestDiscoverCodexHomesRejectsFilesAndSymlinks(t *testing.T) {
	home := discoveryHome(t)
	file := filepath.Join(home, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", file)
	items, err := DiscoverCodexHomes(t.Context())
	if err != nil || items[0].Status != "unavailable" {
		t.Fatalf("file was accepted: %+v err=%v", items, err)
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(home, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	t.Setenv("CODEX_HOME", link)
	items, err = DiscoverCodexHomes(t.Context())
	if err != nil || items[0].Status != "unavailable" {
		t.Fatalf("symlink was followed: %+v err=%v", items, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := DiscoverCodexHomes(ctx); err == nil {
		t.Fatal("cancelled discovery was allowed")
	}
}
