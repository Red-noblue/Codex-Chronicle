// 中文说明：Windows 目录移动使用 MoveFileW；目标存在时系统会拒绝替换。
//go:build windows

package platform

import "golang.org/x/sys/windows"

func renamePathNoReplace(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFile(from, to)
}
