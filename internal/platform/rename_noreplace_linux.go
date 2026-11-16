// 中文说明：Linux 目录移动使用 renameat2 的 NOREPLACE 标志，避免替换已有回收站项目。
//go:build linux

package platform

import "golang.org/x/sys/unix"

func renamePathNoReplace(source, target string) error {
	return unix.Renameat2(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE)
}
