// 中文说明：验证 Win32 结构 ABI，并在独立窗口中实际运行设置消息循环及关闭流程，不创建登录项。
package desktopui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"codex-chronicle.local/internal/desktop"
)

func TestWindowsNativeABI(t *testing.T) {
	if unsafe.Sizeof(windowClass{}) != 80 || unsafe.Sizeof(winMessage{}) != 48 || unsafe.Sizeof(iconData{}) != 976 {
		t.Fatalf("原生结构尺寸不符：WNDCLASSEX=%d MSG=%d NOTIFYICONDATA=%d", unsafe.Sizeof(windowClass{}), unsafe.Sizeof(winMessage{}), unsafe.Sizeof(iconData{}))
	}
}

func TestWindowsSettingsWindow(t *testing.T) {
	dir := t.TempDir()
	configure("", "test-launcher.exe", dir, dir)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		deadline := time.Now().Add(10 * time.Second)
		for activeWindow.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		Stop()
	}()
	runWindows(true)
	<-closed
	settings, err := desktop.LoadStartupSettings(dir)
	if err != nil || settings.Port != 8787 || !settings.AutoFallback {
		t.Fatalf("关闭设置窗口不应改动配置：%+v %v", settings, err)
	}
}

func TestWindowsSettingsSaveAndValidation(t *testing.T) {
	dir := t.TempDir()
	configure("", "test-launcher.exe", dir, dir)
	checked := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		var hwnd uintptr
		for time.Now().Before(deadline) {
			hwnd, _, _ = user32.NewProc("FindWindowW").Call(uintptr(unsafe.Pointer(utf16("CodexChronicleTrayWindow"))), uintptr(unsafe.Pointer(utf16("端口设置 · Codex Chronicle"))))
			if hwnd != 0 {
				// 消息循环就绪后，跨线程 SendMessage 才会完成。
				sendMessage.Call(hwnd, 0, 0, 0)
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if hwnd == 0 {
			checked <- fmt.Errorf("未创建原生端口窗口")
			Stop()
			return
		}
		field, _, _ := user32.NewProc("GetDlgItem").Call(hwnd, 201)
		setWindowText.Call(field, uintptr(unsafe.Pointer(utf16("0"))))
		sendMessage.Call(hwnd, 0x0111, cmdSave, 0)
		if _, err := os.Stat(filepath.Join(dir, "startup.json")); !os.IsNotExist(err) {
			checked <- fmt.Errorf("非法端口不应生成设置：%v", err)
			Stop()
			return
		}
		setWindowText.Call(field, uintptr(unsafe.Pointer(utf16("9321"))))
		checkbox, _, _ := user32.NewProc("GetDlgItem").Call(hwnd, 202)
		sendMessage.Call(checkbox, 0x00f1, 0, 0)
		sendMessage.Call(hwnd, 0x0111, cmdSave, 0)
		checked <- nil
	}()
	runWindows(true)
	if err := <-checked; err != nil {
		t.Fatal(err)
	}
	settings, err := desktop.LoadStartupSettings(dir)
	if err != nil || settings.Port != 9321 || settings.AutoFallback {
		t.Fatalf("原生控件的保存行为错误：%+v %v", settings, err)
	}
}
