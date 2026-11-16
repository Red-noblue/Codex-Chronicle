//go:build darwin && cgo

// 中文说明：使用系统 AppKit 提供轻量菜单栏入口，打开浏览器和等待任务完成后退出后台。
package desktopui

/*
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>
void chronicleMenu(const char *url, const char *executable, const char *dataDir);
void chronicleStopMenu(void);
void chronicleShowError(const char *message);
*/
import "C"

import (
	"runtime"
	"unsafe"
)

func init() { runtime.LockOSThread() }

func Run(url, executable, dataDir string) {
	u, e, d := C.CString(url), C.CString(executable), C.CString(dataDir)
	defer C.free(unsafe.Pointer(u))
	defer C.free(unsafe.Pointer(e))
	defer C.free(unsafe.Pointer(d))
	C.chronicleMenu(u, e, d)
}

func Stop() { C.chronicleStopMenu() }

func ShowError(message string) {
	m := C.CString(message)
	defer C.free(unsafe.Pointer(m))
	C.chronicleShowError(m)
}
