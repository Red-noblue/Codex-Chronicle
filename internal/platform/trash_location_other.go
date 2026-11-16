//go:build !darwin || !cgo

// 中文说明：其他平台只返回约定候选；Windows 的真实目的地由 Shell 的移动回执确定。
package platform

import "fmt"

func NativeTrashDirectory(itemPath string) (string, error) {
	detection := DetectSystemTrash()
	if detection.CandidatePath == "" {
		return "", fmt.Errorf("系统未提供回收站候选位置")
	}
	return detection.CandidatePath, nil
}
