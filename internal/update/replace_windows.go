// 中文说明：在 Windows 平台使用 MoveFileEx 的替换语义更新控制文件。
//go:build windows

package update

import (
	"fmt"
	"golang.org/x/sys/windows"
)

func replaceFile(source, destination string) error {
	sourcePath, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPath, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(sourcePath, destinationPath, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return fmt.Errorf("Windows 原子替换失败: %w", err)
	}
	return nil
}
