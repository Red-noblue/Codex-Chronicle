// 中文说明：使用 Windows 内核锁保护跨进程更新和活跃下载，强制退出后由系统释放。
package update

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func lockUpdateFile(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errUpdateBusy
	}
	return err
}
