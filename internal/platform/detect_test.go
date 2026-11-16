// 中文说明：验证平台回收站探针只读取路径，不创建目录或文件。
package platform

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectTrashForDarwinReportsExistingDirectory(t *testing.T) {
	home := t.TempDir()
	trash := filepath.Join(home, ".Trash")
	if err := os.Mkdir(trash, 0o700); err != nil {
		t.Fatal(err)
	}
	result := DetectTrashFor("darwin", "arm64", home)
	if !result.ReadOnly || !result.Exists || !result.IsDirectory || !result.Supported || result.CandidatePath != trash {
		t.Fatalf("unexpected trash detection: %+v", result)
	}
}

func TestDetectTrashForWindowsReportsNativeProviderCandidate(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "Recycle.Bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	result := DetectTrashFor("windows", "amd64", home)
	if !result.ReadOnly || !result.Supported || !result.Exists {
		t.Fatalf("unexpected Windows detection: %+v", result)
	}
}

func TestDetectTrashForUnknownPlatformDoesNotWrite(t *testing.T) {
	home := t.TempDir()
	before, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	result := DetectTrashFor("unsupported-os", "unknown-arch", home)
	after, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if result.Supported || len(before) != len(after) {
		t.Fatalf("unexpected unknown platform result: %+v", result)
	}
}
