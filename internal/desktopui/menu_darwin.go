//go:build darwin && cgo

// 中文说明：使用系统 AppKit 提供轻量菜单栏入口，打开浏览器和等待任务完成后退出后台。
package desktopui

/*
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>
void chronicleMenu(const char *url);
void chronicleStopMenu(void);
void chronicleShowError(const char *message);
void chronicleSettings(void);
void chronicleStartupError(const char *message);
*/
import "C"

import (
	"encoding/json"
	"runtime"
	"unsafe"

	"codex-chronicle.local/internal/desktop"
)

func init() { runtime.LockOSThread() }

func Run(url, executable, dataDir, packageRoot string) {
	guard, err := desktop.AcquireTray(dataDir)
	if err != nil {
		if err != desktop.ErrAlreadyRunning {
			ShowError(err.Error())
		}
		return
	}
	defer guard.Close()
	configure(url, executable, dataDir, packageRoot)
	u := C.CString(url)
	defer C.free(unsafe.Pointer(u))
	C.chronicleMenu(u)
}

func Stop() { C.chronicleStopMenu() }

func ShowError(message string) {
	m := C.CString(message)
	defer C.free(unsafe.Pointer(m))
	C.chronicleShowError(m)
}

func Settings(executable, dataDir, packageRoot string) {
	configure("", executable, dataDir, packageRoot)
	C.chronicleSettings()
}

func ShowStartupError(message, executable, dataDir, packageRoot string) {
	configure("", executable, dataDir, packageRoot)
	m := C.CString(message)
	defer C.free(unsafe.Pointer(m))
	C.chronicleStartupError(m)
}

//export chronicleAction
func chronicleAction(action, value *C.char, enabled C.int) *C.char {
	var err error
	result := map[string]any{}
	switch C.GoString(action) {
	case "state":
		settings, login, stateErr := controller.settings()
		result["port"], result["auto_fallback"], result["autostart"], result["url"] = settings.Port, settings.AutoFallback, login, controller.url
		err = stateErr
	case "port":
		err = controller.savePort(C.GoString(value), enabled != 0)
	case "fallback":
		err = controller.toggleFallback()
	case "autostart":
		err = controller.toggleAutostart()
	case "logs":
		err = controller.logs()
	case "quit":
		err = controller.quit()
	}
	if err != nil {
		result["error"] = err.Error()
	}
	data, _ := json.Marshal(result)
	return C.CString(string(data))
}
