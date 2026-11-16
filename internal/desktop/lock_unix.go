//go:build !windows

// 中文说明：使用 Unix 内核锁保证同一用户数据目录只被一个安装版后台占用。
package desktop

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockInstance(file *os.File) error {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrAlreadyRunning
	}
	return err
}
