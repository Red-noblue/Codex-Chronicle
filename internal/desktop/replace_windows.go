//go:build windows

// 中文说明：使用 Windows 原子替换重命名更新现有启动配置，不删除旧设置作为中间步骤。
package desktop

import "golang.org/x/sys/windows"

func replaceStartupFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
