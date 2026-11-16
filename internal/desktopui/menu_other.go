//go:build !darwin || !cgo

// 中文说明：非 macOS 平台保持浏览器启动方式，后台可通过启动器 --quit 安全退出。
package desktopui

func Run(url, executable, dataDir string) {}

func Stop() {}

func ShowError(message string) {}
