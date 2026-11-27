//go:build !darwin || !cgo

// 中文说明：非 macOS 原生构建不会显示拖放安装提示。
package desktopui

func ShowInstallRequired() {}
