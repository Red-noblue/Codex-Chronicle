// 中文说明：验证目录同步不掩盖目标缺失或普通文件错误。
package durablefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSyncDirectoryValidatesTarget(t *testing.T) {
	root := t.TempDir()
	if err := SyncDirectory(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "file")
	if err := SyncDirectory(path); !os.IsNotExist(err) {
		t.Fatalf("目标缺失时应返回错误: %v", err)
	}
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncDirectory(path); err == nil {
		t.Fatal("普通文件不应当作目录同步")
	}
}
