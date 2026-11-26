// 中文说明：验证空间估算的安全余量、普通用户配额和空间不足时不改变安装状态。
package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRequireDiskSpaceUsesAvailableQuotaAndReserve(t *testing.T) {
	root := t.TempDir()
	query := func(path string) (uint64, error) {
		if path != root {
			t.Fatalf("没有找到目标所在卷：%s", path)
		}
		return updateSpaceReserve + 100, nil
	}
	if err := requireDiskSpace(filepath.Join(root, "new", "payload"), 100, "测试", query); err != nil {
		t.Fatal(err)
	}
	var space *InsufficientSpaceError
	if err := requireDiskSpace(root, 101, "测试", query); !errors.As(err, &space) || space.RequiredBytes != updateSpaceReserve+101 {
		t.Fatalf("空间不足未准确报告：%v", err)
	}
	if err := RequireDiskSpace(root, 0, "测试"); err != nil {
		t.Fatal(err)
	}
}

func TestNoSpaceBlocksExtractionAndActivationBeforeWriting(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := m.Prepare(t.Context(), makeBundle(t, "1.0.0", "旧程序"), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Activate(t.Context(), plan, m.VerifyVersionRoot); err != nil {
		t.Fatal(err)
	}
	next := makeBundle(t, "2.0.0", "新程序")
	plan, err = m.Prepare(t.Context(), next, false)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := os.ReadFile(filepath.Join(next, ManifestName))
	archive := filepath.Join(t.TempDir(), "用户离线包.zip")
	if err := writeZIP(archive, map[string][]byte{ManifestName: manifest, "bin/codex-chronicle": []byte("新程序")}); err != nil {
		t.Fatal(err)
	}
	m.diskSpaceCheck = func(string, int64, string) error { return &InsufficientSpaceError{Phase: "测试"} }
	if _, err := m.PrepareArchive(t.Context(), archive, false); err == nil {
		t.Fatal("空间不足仍展开包")
	}
	called := false
	if _, err := m.Activate(t.Context(), plan, func(_ context.Context, _ string) error { called = true; return nil }); err == nil || called {
		t.Fatal("空间不足仍复制或试启动")
	}
	status, err := m.Status(t.Context())
	if err != nil || status.State.ActiveVersion != "1.0.0" || status.RecoveryRequired {
		t.Fatalf("空间不足污染了旧版本：%+v %v", status, err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Fatal("用户离线包被删除")
	}
}
