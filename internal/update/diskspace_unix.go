//go:build !windows

// 中文说明：读取 Unix 文件系统留给当前普通用户的可用空间，不把保留给管理员的空间计入。
package update

import "golang.org/x/sys/unix"

func availableDiskSpace(path string) (uint64, error) {
	var info unix.Statfs_t
	if err := unix.Statfs(path, &info); err != nil {
		return 0, err
	}
	return uint64(info.Bavail) * uint64(info.Bsize), nil
}
