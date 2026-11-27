//go:build darwin && cgo

// 中文说明：从磁盘镜像直接打开应用时显示原生拖放安装指引，不启动终端或创建后台。
package desktopui

/*
void chronicleInstallRequired(void);
*/
import "C"

func ShowInstallRequired() { C.chronicleInstallRequired() }
