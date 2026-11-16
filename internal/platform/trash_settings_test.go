// 中文说明：验证回收站路径设置的持久化、路径约束和只读探测边界。
package platform

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrashSettingsStoreRoundTripAndReset(t *testing.T) {
	base := t.TempDir()
	settingsPath := filepath.Join(base, "config", "trash-settings.json")
	custom := filepath.Join(base, "Trash", "files")
	if err := os.MkdirAll(custom, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewTrashSettingsStore(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), TrashSettings{CustomPath: custom}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background())
	if err != nil || loaded.CustomPath != custom || loaded.Version != trashSettingsVersion {
		t.Fatalf("unexpected persisted settings: %+v err=%v", loaded, err)
	}
	if err := store.Save(context.Background(), TrashSettings{Version: trashSettingsVersion}); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Load(context.Background())
	if err != nil || loaded.CustomPath != "" {
		t.Fatalf("settings reset failed: %+v err=%v", loaded, err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil || !strings.Contains(string(data), `"version": 1`) {
		t.Fatalf("settings file is not readable JSON: %q err=%v", data, err)
	}
}

func TestTrashSettingsRejectsCodexAndInvalidLinuxPaths(t *testing.T) {
	base := t.TempDir()
	store, err := NewTrashSettingsStore(filepath.Join(base, "config", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), TrashSettings{CustomPath: filepath.Join(base, ".codex", "Trash", "files")}); err == nil {
		t.Fatal("expected .codex path rejection")
	}
	validButWrong := filepath.Join(base, "Trash")
	if err := os.MkdirAll(validButWrong, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := validateTrashSettings(TrashSettings{CustomPath: validButWrong}, "linux", "arm64"); err == nil {
		t.Fatal("expected Linux Trash/files shape rejection")
	}
}

func TestDetectTrashAtPathIsReadOnly(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "Trash", "files")
	result := DetectTrashAtPath("linux", "arm64", path)
	if result.Supported || result.Exists {
		t.Fatalf("missing custom path should not be reported as supported: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(base, "Trash")); !os.IsNotExist(err) {
		t.Fatalf("custom path probe created a directory: %v", err)
	}
}
