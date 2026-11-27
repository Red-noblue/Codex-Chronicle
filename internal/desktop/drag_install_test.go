// 中文说明：验证拖放安装位置检查只约束 macOS 应用包，普通目录中的已复制应用可以正常启动。
package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDragInstallAllowsCopiedAppAndPortableLauncher(t *testing.T) {
	app := filepath.Join(t.TempDir(), "Codex Chronicle.app", "Contents", "MacOS")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{filepath.Join(app, "Codex Chronicle"), filepath.Join(t.TempDir(), "bin", "codex-chronicle-desktop")} {
		if required, err := DragInstallRequired(executable); err != nil || required {
			t.Fatalf("可写目录中的应用或便携入口被阻断：%v %v", required, err)
		}
	}
}

func TestDragInstallRejectsUnknownAppVolumeOnMac(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("仅检查 macOS 应用所在卷")
	}
	executable := filepath.Join(t.TempDir(), "missing.app", "Contents", "MacOS", "Codex Chronicle")
	if _, err := DragInstallRequired(executable); err == nil {
		t.Fatal("无法确认应用所在卷时继续了启动")
	}
}
