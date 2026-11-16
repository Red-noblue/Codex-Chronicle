// 中文说明：在 Unix 平台使用同目录 rename 原子替换更新控制文件。
//go:build !windows

package update

import "os"

func replaceFile(source, destination string) error {
	return os.Rename(source, destination)
}
