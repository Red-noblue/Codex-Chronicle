//go:build !darwin

// 中文说明：拖放安装位置检查仅作用于 macOS，其他平台保持原启动方式。
package desktop

func DragInstallRequired(executable string) (bool, error) { return false, nil }
