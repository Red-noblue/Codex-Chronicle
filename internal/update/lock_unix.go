//go:build !windows

// 中文说明：使用进程退出后自动释放的内核锁，保护更新事务和正在下载的临时文件。
package update

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func lockUpdateFile(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return errUpdateBusy
	}
	return err
}
