//go:build darwin && !cgo

// 中文说明：macOS 禁用 cgo 时回退到系统回收站目录 provider，保持纯 Go 构建能力。
package platform

func newNativeSystemTrash() (TrashProvider, error) {
	return NewSystemTrash()
}
