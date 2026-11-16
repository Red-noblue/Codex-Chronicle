// 中文说明：使用 Windows 内核区域锁保证同一用户数据目录只有一个后台实例。
package desktop

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockInstance(file *os.File) error {
	err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrAlreadyRunning
	}
	return err
}
