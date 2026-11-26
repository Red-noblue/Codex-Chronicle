// 中文说明：验证启动器在首次启动、更新激活和异常版本指针下选择正确的发布载荷。
package desktop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestActivePackageRootUsesActivatedVersion(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	initial, err := ActivePackageRoot(root, data)
	if err != nil || initial != root {
		t.Fatalf("首次启动没有使用安装载荷：%q %v", initial, err)
	}
	active := filepath.Join(data, "updates", "versions", "1.2.3")
	if err := os.MkdirAll(active, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(data, "updates", "install-state.json")
	state, err := json.Marshal(map[string]string{"active_version": "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, state, 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := ActivePackageRoot(root, data)
	if err != nil || selected != active {
		t.Fatalf("激活版本没有被选择：%q %v", selected, err)
	}
}

func TestActivePackageRootRejectsInvalidOrMissingVersion(t *testing.T) {
	root := t.TempDir()
	data := t.TempDir()
	updates := filepath.Join(data, "updates")
	if err := os.MkdirAll(updates, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"../escape", "a/b", "missing"} {
		state, err := json.Marshal(map[string]string{"active_version": version})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(updates, "install-state.json"), state, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ActivePackageRoot(root, data); err == nil {
			t.Fatalf("无效或缺失激活版本未被拒绝：%q", version)
		}
	}
	link := filepath.Join(updates, "versions", "linked")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("当前平台不支持符号链接：%v", err)
	}
	state, err := json.Marshal(map[string]string{"active_version": "linked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(updates, "install-state.json"), state, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivePackageRoot(root, data); err == nil {
		t.Fatal("符号链接激活版本未被拒绝")
	}
}
