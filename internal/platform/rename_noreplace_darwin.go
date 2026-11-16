// 中文说明：macOS 目录移动使用 RENAME_EXCL，避免替换已有回收站项目。
//go:build darwin

package platform

import "golang.org/x/sys/unix"

func renamePathNoReplace(source, target string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, target, unix.RENAME_EXCL)
}
