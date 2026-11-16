// 中文说明：验证首次默认定位、工作区重启恢复和多实例配置隔离；所有目录均为合成数据。
package main

import (
	"os"
	"path/filepath"
	"testing"

	"codex-chronicle.local/internal/api"
	"codex-chronicle.local/internal/workspace"
)

func TestWorkspaceStartupDiscoversAndPersistsFirstSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "portable"))
	root, config, err := resolveWorkspaceStartup(t.Context(), "", "", "")
	if err != nil || root != "" || config == "" {
		t.Fatalf("first run without Codex should allow manual add: %q %q %v", root, config, err)
	}
	if _, err := os.Stat(filepath.Dir(config)); !os.IsNotExist(err) {
		t.Fatal("resolving defaults wrote configuration")
	}
	defaultRoot := filepath.Join(home, ".codex")
	if err := os.MkdirAll(filepath.Join(defaultRoot, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, config, err = resolveWorkspaceStartup(t.Context(), "", "", "")
	if err != nil || root != defaultRoot {
		t.Fatalf("missing CODEX_HOME should fall back to .codex: %q %v", root, err)
	}
	if _, err := api.NewServerWithWorkspaceConfig(root, nil, nil, config); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(os.Getenv("CODEX_HOME"), "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	restored, sameConfig, err := resolveWorkspaceStartup(t.Context(), "", "", "")
	if err != nil || restored != defaultRoot || sameConfig != config {
		t.Fatalf("newly discovered source overrode saved choice: %q %q %v", restored, sameConfig, err)
	}
	// 首次没有保存选择时，有效的 CODEX_HOME 优先。
	portable, _, err := resolveWorkspaceStartup(t.Context(), "", "", filepath.Join(home, "fresh.json"))
	if err != nil || portable != os.Getenv("CODEX_HOME") {
		t.Fatalf("CODEX_HOME priority lost: %q %v", portable, err)
	}
}

func TestWorkspaceStartupKeepsInstancesAndExplicitPathsSeparate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("APPDATA", filepath.Join(home, "config"))
	a, b := filepath.Join(home, "a"), filepath.Join(home, "b")
	for _, root := range []string{a, b} {
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	_, configA, err := resolveWorkspaceStartup(t.Context(), a, "", "")
	if err != nil {
		t.Fatal(err)
	}
	root, configB, err := resolveWorkspaceStartup(t.Context(), b, "", "")
	if err != nil || root != b || configA == configB {
		t.Fatalf("different scan roots shared defaults: %q %q %v", configA, configB, err)
	}
	archive := filepath.Join(home, "archive.db")
	_, config, err := resolveWorkspaceStartup(t.Context(), a, archive, "")
	if err != nil || config != archive+".workspaces.json" {
		t.Fatalf("archive-local default missing: %q %v", config, err)
	}
	file, err := workspace.NewFileStore(config)
	if err != nil {
		t.Fatal(err)
	}
	registry := workspace.Registry{Version: 1}
	if _, err := workspace.Add(t.Context(), &registry, workspace.Definition{Root: b}); err != nil {
		t.Fatal(err)
	}
	if err := file.Save(t.Context(), registry); err != nil {
		t.Fatal(err)
	}
	root, _, err = resolveWorkspaceStartup(t.Context(), a, archive, config)
	if err != nil || root != b {
		t.Fatalf("saved selection was overwritten: %q %v", root, err)
	}
	if _, _, err := resolveWorkspaceStartup(t.Context(), a, "", filepath.Join(a, "workspaces.json")); err == nil {
		t.Fatal("registry inside source was accepted")
	}
}
