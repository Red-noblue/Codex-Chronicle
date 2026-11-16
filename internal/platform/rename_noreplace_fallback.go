// 中文说明：其他 Unix 平台在目标不存在时使用系统重命名完成目录移动。
//go:build !linux && !darwin && !windows

package platform

import "fmt"

func renamePathNoReplace(source, target string) error {
	return fmt.Errorf("当前平台不支持无覆盖目录移动")
}
