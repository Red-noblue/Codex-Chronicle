// 中文说明：非 Windows 平台的原生回收站入口复用现有系统目录 provider。
//go:build !windows && !darwin

package platform

func newNativeSystemTrash() (TrashProvider, error) {
	return NewSystemTrash()
}
