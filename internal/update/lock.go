// 中文说明：更新事务使用独占锁，下载使用各自租约；恢复只清理已失去租约的本项目半成品。
package update

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errUpdateBusy = errors.New("其他进程仍在执行更新，请完成后重试")

func acquireUpdateLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("更新锁不是普通文件")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockUpdateFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (m *Manager) operationLock() (*os.File, error) {
	return acquireUpdateLock(filepath.Join(m.root, ".operation.lock"))
}

func (m *Manager) recoverDownloads(ctx context.Context) error {
	root := filepath.Join(m.root, BundlesDirName)
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name(), ".lock")
		if !strings.HasSuffix(name, ".archive.tmp") || !strings.HasPrefix(name, "download-") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, "download-"), ".archive.tmp")
		if decoded, err := hex.DecodeString(id); err != nil || len(decoded) != 12 || seen[name] {
			continue
		}
		seen[name] = true
		temporary := filepath.Join(root, name)
		lock, err := acquireUpdateLock(temporary + ".lock")
		if errors.Is(err, errUpdateBusy) {
			continue
		}
		if err != nil {
			return err
		}
		if info, err := os.Lstat(temporary); err == nil && !info.Mode().IsRegular() {
			_ = lock.Close()
			return fmt.Errorf("下载半成品不是普通文件：%s", name)
		}
		removeErr := os.Remove(temporary)
		_ = lock.Close()
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		if err := os.Remove(temporary + ".lock"); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
