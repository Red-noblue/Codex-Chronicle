// 中文说明：使用 Windows 卷配额感知接口读取当前用户可用磁盘空间。
package update

import "golang.org/x/sys/windows"

func availableDiskSpace(path string) (uint64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	err = windows.GetDiskFreeSpaceEx(name, &available, &total, &free)
	return available, err
}
