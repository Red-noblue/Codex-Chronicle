// 中文说明：使用 Windows 原生通知区域、菜单和设置窗口，无 WebView 和额外监听端口；左键打开网页，右键提供后台管理。
package desktopui

import (
	"fmt"
	"runtime"
	"strconv"
	"sync/atomic"
	"unsafe"

	"codex-chronicle.local/internal/desktop"
	"golang.org/x/sys/windows"
)

var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	shell32          = windows.NewLazySystemDLL("shell32.dll")
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	gdi32            = windows.NewLazySystemDLL("gdi32.dll")
	moveMemory       = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlMoveMemory")
	registerClass    = user32.NewProc("RegisterClassExW")
	createWindow     = user32.NewProc("CreateWindowExW")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	destroyWindow    = user32.NewProc("DestroyWindow")
	getMessage       = user32.NewProc("GetMessageW")
	translateMessage = user32.NewProc("TranslateMessage")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
	postMessage      = user32.NewProc("PostMessageW")
	postQuit         = user32.NewProc("PostQuitMessage")
	sendMessage      = user32.NewProc("SendMessageW")
	notifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	appendMenu       = user32.NewProc("AppendMenuW")
	getCursorPos     = user32.NewProc("GetCursorPos")
	setForeground    = user32.NewProc("SetForegroundWindow")
	createPopup      = user32.NewProc("CreatePopupMenu")
	trackPopup       = user32.NewProc("TrackPopupMenuEx")
	destroyMenu      = user32.NewProc("DestroyMenu")
	messageBox       = user32.NewProc("MessageBoxW")
	setWindowText    = user32.NewProc("SetWindowTextW")
	getWindowText    = user32.NewProc("GetWindowTextW")
	setWindowPos     = user32.NewProc("SetWindowPos")
	showWindow       = user32.NewProc("ShowWindow")
	getDpi           = user32.NewProc("GetDpiForWindow")
	isDialogMessage  = user32.NewProc("IsDialogMessageW")
	setFocus         = user32.NewProc("SetFocus")
)

const (
	wmTray      = 0x8001
	wmQuitDone  = 0x8002
	wmStop      = 0x8003
	cmdOpen     = 101
	cmdCopy     = 102
	cmdLogin    = 103
	cmdPort     = 104
	cmdFallback = 105
	cmdLogs     = 106
	cmdQuit     = 107
	cmdUpdates  = 108
	cmdDownload = 109
	cmdSave     = 1
	cmdCancel   = 2
	wsChild     = 0x40000000
	wsVisible   = 0x10000000
	wsTabStop   = 0x00010000
)

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
type winMessage struct {
	Window         uintptr
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Point          point
	Private        uint32
}
type windowClass struct {
	Size, Style                        uint32
	Proc                               uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	Menu, Name                         *uint16
	SmallIcon                          uintptr
}
type iconData struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Version             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                windows.GUID
	BalloonIcon         uintptr
}
type controlLayout struct {
	hwnd       uintptr
	x, y, w, h int
}
type portDialog struct {
	hwnd, port, fallback, errorLabel uintptr
	controls                         []controlLayout
	font                             uintptr
}
type windowsUI struct {
	hwnd, icon           uintptr
	taskbarCreated       uint32
	notification         iconData
	stopping, standalone bool
	dialog               *portDialog
	quitResult           chan error
}

var activeWindow atomic.Uintptr
var ui *windowsUI // 仅在锁定的界面线程读写；跨线程退出通过窗口消息通知。
var windowCallback = windows.NewCallback(windowProc)

func utf16(value string) *uint16 { p, _ := windows.UTF16PtrFromString(value); return p }
func signed(value int32) uintptr { return uintptr(value) }

func ShowError(message string) {
	messageBox.Call(0, uintptr(unsafe.Pointer(utf16(message))), uintptr(unsafe.Pointer(utf16("Codex Chronicle"))), 0x10)
}

func ShowStartupError(message, executable, dataDir, packageRoot string) {
	text := message + "\n\n是否打开端口设置？修改后重新打开应用即可。"
	answer, _, _ := messageBox.Call(0, uintptr(unsafe.Pointer(utf16(text))), uintptr(unsafe.Pointer(utf16("Codex Chronicle 暂时无法打开"))), 0x14)
	if answer == 6 {
		Settings(executable, dataDir, packageRoot)
	}
}

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
	runWindows(false)
}

func Settings(executable, dataDir, packageRoot string) {
	configure("", executable, dataDir, packageRoot)
	runWindows(true)
}

func Stop() {
	if hwnd := activeWindow.Load(); hwnd != 0 {
		postMessage.Call(hwnd, wmStop, 0, 0)
	}
}

func runWindows(standalone bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	releaseStyles := activateVisualStyles()
	defer releaseStyles()
	// 原生菜单跟随显示器缩放；设置窗口在 WM_DPICHANGED 时重新排版。
	dpiContext := user32.NewProc("SetThreadDpiAwarenessContext")
	if dpiContext.Find() == nil {
		dpiContext.Call(^uintptr(3))
	}
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
	className := utf16("CodexChronicleTrayWindow")
	class := windowClass{Proc: windowCallback, Instance: uintptr(instance), Cursor: cursor, Background: 6, Name: className}
	class.Size = uint32(unsafe.Sizeof(class))
	registered, _, err := registerClass.Call(uintptr(unsafe.Pointer(&class)))
	if registered == 0 {
		ShowError(fmt.Sprintf("无法注册托盘窗口：%v", err))
		return
	}
	defer user32.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(className)), uintptr(instance))
	ui = &windowsUI{standalone: standalone, quitResult: make(chan error, 1)}
	hwnd, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(utf16("Codex Chronicle"))), 0, 0, 0, 0, 0, 0, 0, uintptr(instance), 0)
	if hwnd == 0 {
		ShowError(fmt.Sprintf("无法创建托盘窗口：%v", err))
		return
	}
	ui.hwnd = hwnd
	activeWindow.Store(hwnd)
	defer activeWindow.Store(0)
	taskbarMessage, _, _ := user32.NewProc("RegisterWindowMessageW").Call(uintptr(unsafe.Pointer(utf16("TaskbarCreated"))))
	ui.taskbarCreated = uint32(taskbarMessage)
	if standalone {
		ui.showSettings()
	} else {
		user32.NewProc("SetTimer").Call(hwnd, 2, 3000, 0)
		ui.icon = archiveIcon()
		defer user32.NewProc("DestroyIcon").Call(ui.icon)
		ui.notification = iconData{Window: hwnd, ID: 1, Flags: 1 | 2 | 4 | 0x80, Callback: wmTray, Icon: ui.icon, Version: 4}
		ui.notification.Size = uint32(unsafe.Sizeof(ui.notification))
		copy(ui.notification.Tip[:], windows.StringToUTF16("Codex Chronicle · 运行中\n"+controller.url))
		if !ui.addIcon() {
			// 登录时 Explorer 可能尚未就绪；等待 Shell 恢复，不因暂时失败留下不可管理的后台。
			user32.NewProc("SetTimer").Call(hwnd, 1, 2000, 0)
		}
	}
	var message winMessage
	for {
		result, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		if int32(result) <= 0 {
			break
		}
		if ui.dialog != nil {
			if message.Message == 0x0100 && (message.WParam == 13 || message.WParam == 27) {
				if message.WParam == 13 {
					ui.saveSettings()
				} else {
					ui.closeSettings()
				}
				continue
			}
			handled, _, _ := isDialogMessage.Call(ui.dialog.hwnd, uintptr(unsafe.Pointer(&message)))
			if handled != 0 {
				continue
			}
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&message)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
	}
	if !standalone {
		notifyIcon.Call(2, uintptr(unsafe.Pointer(&ui.notification)))
	}
}

func (w *windowsUI) addIcon() bool {
	added, _, _ := notifyIcon.Call(0, uintptr(unsafe.Pointer(&w.notification)))
	if added != 0 {
		notifyIcon.Call(4, uintptr(unsafe.Pointer(&w.notification)))
		user32.NewProc("KillTimer").Call(w.hwnd, 1)
	}
	return added != 0
}

func windowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	if ui != nil {
		if ui.dialog != nil && hwnd == ui.dialog.hwnd {
			switch message {
			case 0x0010:
				ui.closeSettings()
				return 0 // WM_CLOSE
			case 0x0111: // WM_COMMAND
				switch wParam & 0xffff {
				case cmdSave:
					ui.saveSettings()
				case cmdCancel:
					ui.closeSettings()
				}
				return 0
			case 0x02e0: // WM_DPICHANGED
				var recommended rect
				moveMemory.Call(uintptr(unsafe.Pointer(&recommended)), lParam, unsafe.Sizeof(recommended))
				setWindowPos.Call(hwnd, 0, signed(recommended.Left), signed(recommended.Top), signed(recommended.Right-recommended.Left), signed(recommended.Bottom-recommended.Top), 0x14)
				ui.layoutDialog(int(wParam & 0xffff))
				return 0
			}
		}
		if hwnd == ui.hwnd {
			if message == ui.taskbarCreated && ui.taskbarCreated != 0 {
				ui.addIcon()
				return 0
			}
			switch message {
			case 0x0113: // WM_TIMER：图标恢复和更新状态分别使用固定编号。
				if wParam == 1 {
					ui.addIcon()
				} else if wParam == 2 && !ui.standalone && !ui.stopping {
					ui.tooltip(controller.updateState().Label)
				}
				return 0
			case wmTray:
				switch uint32(lParam & 0xffff) {
				case 0x0400, 0x0401: // NIN_SELECT / NIN_KEYSELECT，保留键盘激活。
					if !ui.stopping {
						if err := controller.open(); err != nil {
							ShowError(err.Error())
						}
					}
				case 0x007b:
					ui.menu() // WM_CONTEXTMENU，鼠标右键及键盘菜单键。
				}
				return 0
			case wmStop:
				if ui.dialog != nil {
					ui.closeSettings()
				}
				destroyWindow.Call(hwnd)
				return 0
			case wmQuitDone:
				if err := <-ui.quitResult; err != nil {
					ui.stopping = false
					ui.tooltip("运行中\n" + controller.url)
					ShowError(err.Error())
				} else {
					destroyWindow.Call(hwnd)
				}
				return 0
			case 0x0010: // 后台只能从安全退出动作关闭，不能通过窗口默认行为误杀。
				ui.quit()
				return 0
			case 0x0002:
				if ui.dialog != nil {
					ui.closeSettings()
				}
				postQuit.Call(0)
				return 0
			}
		}
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func (w *windowsUI) menu() {
	menu, _, _ := createPopup.Call()
	if menu == 0 {
		return
	}
	defer destroyMenu.Call(menu)
	add := func(id uintptr, title string, flags uintptr) {
		appendMenu.Call(menu, flags, id, uintptr(unsafe.Pointer(utf16(title))))
	}
	separator := func() { appendMenu.Call(menu, 0x800, 0, 0) }
	settings, login, err := controller.settings()
	add(0, "Codex Chronicle", 2)
	status := "后台运行中"
	if w.stopping {
		status = "正在等待任务完成…"
	}
	add(0, status, 2)
	add(0, controller.url, 2)
	separator()
	var openFlags uintptr
	if w.stopping {
		openFlags = 2
	}
	add(cmdOpen, "打开工作台", openFlags)
	user32.NewProc("SetMenuDefaultItem").Call(menu, cmdOpen, 0)
	add(cmdCopy, "复制工作台地址", 0)
	separator()
	var checked uintptr
	if login {
		checked = 8
	}
	add(cmdLogin, "登录时启动", checked)
	add(cmdPort, "端口设置…", 0)
	checked = 0
	if settings.AutoFallback {
		checked = 8
	}
	add(cmdFallback, "端口占用时自动回退", checked)
	add(0, "端口设置下次启动生效", 2)
	if err != nil {
		add(0, "设置读取失败，请打开端口设置修复", 2)
	}
	separator()
	update := controller.updateState()
	add(0, update.Label, 2)
	if update.CanControl {
		title := "暂停更新下载"
		if update.Paused {
			title = "继续下载"
		}
		add(cmdDownload, title, 0)
	}
	add(cmdUpdates, "更新设置…", 0)
	separator()
	add(cmdLogs, "打开日志文件夹", 0)
	checked = 0
	title := "退出 Codex Chronicle"
	if w.stopping {
		checked = 2
		title = "正在安全退出…"
	}
	add(cmdQuit, title, checked)
	var location point
	getCursorPos.Call(uintptr(unsafe.Pointer(&location)))
	setForeground.Call(w.hwnd)
	selected, _, _ := trackPopup.Call(menu, 0x100|0x2, signed(location.X), signed(location.Y), w.hwnd, 0)
	postMessage.Call(w.hwnd, 0, 0, 0) // 按 Shell 规范让菜单在点击外部后正常消失。
	switch selected {
	case cmdOpen:
		err = controller.open()
	case cmdCopy:
		err = clipboardText(w.hwnd, controller.url)
	case cmdLogin:
		err = controller.toggleAutostart()
	case cmdPort:
		w.showSettings()
		return
	case cmdFallback:
		err = controller.toggleFallback()
	case cmdUpdates:
		err = controller.openUpdates()
	case cmdDownload:
		err = controller.toggleDownload()
	case cmdLogs:
		err = controller.logs()
	case cmdQuit:
		w.quit()
		return
	default:
		return
	}
	if err != nil {
		ShowError(err.Error())
	}
}

func (w *windowsUI) quit() {
	if w.stopping {
		return
	}
	w.stopping = true
	w.tooltip("正在等待任务完成…")
	hwnd := w.hwnd
	go func() {
		w.quitResult <- controller.quit()
		postMessage.Call(hwnd, wmQuitDone, 0, 0)
	}()
}

func (w *windowsUI) tooltip(status string) {
	w.notification.Tip = [128]uint16{}
	copy(w.notification.Tip[:], windows.StringToUTF16("Codex Chronicle · "+status))
	notifyIcon.Call(1, uintptr(unsafe.Pointer(&w.notification)))
}

func (w *windowsUI) showSettings() {
	if w.dialog != nil {
		setForeground.Call(w.dialog.hwnd)
		return
	}
	var cursor point
	getCursorPos.Call(uintptr(unsafe.Pointer(&cursor)))
	instance, _, _ := kernel32.NewProc("GetModuleHandleW").Call(0)
	hwnd, _, err := createWindow.Call(0x00000001, uintptr(unsafe.Pointer(utf16("CodexChronicleTrayWindow"))), uintptr(unsafe.Pointer(utf16("端口设置 · Codex Chronicle"))), 0x00c80000, signed(cursor.X-380), signed(cursor.Y-300), 460, 310, w.hwnd, 0, uintptr(instance), 0)
	if hwnd == 0 {
		ShowError(fmt.Sprintf("无法打开端口设置：%v", err))
		return
	}
	w.dialog = &portDialog{hwnd: hwnd}
	control := func(class, text string, style uintptr, id uintptr, x, y, width, height int) uintptr {
		child, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(utf16(class))), uintptr(unsafe.Pointer(utf16(text))), wsChild|wsVisible|style, 0, 0, 0, 0, hwnd, id, uintptr(instance), 0)
		w.dialog.controls = append(w.dialog.controls, controlLayout{child, x, y, width, height})
		return child
	}
	settings, _, loadErr := controller.settings()
	if settings.Port == 0 {
		settings.Port = 8787
	}
	control("STATIC", "网页与 API 共用一个本机端口", 0, 0, 20, 18, 400, 24)
	control("STATIC", "首选端口", 0, 0, 20, 58, 90, 24)
	w.dialog.port = control("EDIT", strconv.Itoa(settings.Port), wsTabStop|0x00800000|0x2000|0x0080, 201, 115, 55, 130, 27)
	sendMessage.Call(w.dialog.port, 0x00c5, 5, 0) // EM_SETLIMITTEXT
	w.dialog.fallback = control("BUTTON", "端口占用时自动回退（推荐）", wsTabStop|3, 202, 20, 96, 395, 24)
	if settings.AutoFallback {
		sendMessage.Call(w.dialog.fallback, 0x00f1, 1, 0)
	}
	hint := "仅监听本机 127.0.0.1；推荐端口 8787。"
	if controller.url != "" {
		hint = "当前地址：" + controller.url
	}
	control("STATIC", hint, 0, 0, 20, 132, 400, 23)
	control("STATIC", "保存后下次启动生效，当前任务继续运行。", 0, 0, 20, 160, 400, 24)
	errorText := ""
	if loadErr != nil {
		errorText = loadErr.Error()
	}
	w.dialog.errorLabel = control("STATIC", errorText, 0, 0, 20, 190, 400, 24)
	control("BUTTON", "保存", wsTabStop|1, cmdSave, 244, 225, 82, 28)
	control("BUTTON", "取消", wsTabStop, cmdCancel, 338, 225, 82, 28)
	dpi, _, _ := getDpi.Call(hwnd)
	if dpi == 0 {
		dpi = 96
	}
	w.layoutDialog(int(dpi))
	showWindow.Call(hwnd, 5)
	setForeground.Call(hwnd)
	setFocus.Call(w.dialog.port)
	sendMessage.Call(w.dialog.port, 0x00b1, 0, ^uintptr(0))
}

func (w *windowsUI) layoutDialog(dpi int) {
	dialog := w.dialog
	scale := func(value int) uintptr { return uintptr(value * dpi / 96) }
	if dialog.font != 0 {
		gdi32.NewProc("DeleteObject").Call(dialog.font)
	}
	font, _, _ := gdi32.NewProc("CreateFontW").Call(signed(-int32(12*dpi/96)), 0, 0, 0, 400, 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(utf16("Microsoft YaHei UI"))))
	dialog.font = font
	for _, control := range dialog.controls {
		setWindowPos.Call(control.hwnd, 0, scale(control.x), scale(control.y), scale(control.w), scale(control.h), 0x14)
		sendMessage.Call(control.hwnd, 0x0030, font, 1)
	}
	// 只调整大小并让 Windows 将弹窗约束在当前显示器的可用区域。
	setWindowPos.Call(dialog.hwnd, 0, 0, 0, scale(460), scale(310), 0x16)
}

func (w *windowsUI) saveSettings() {
	dialog := w.dialog
	var text [16]uint16
	getWindowText.Call(dialog.port, uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)))
	checked, _, _ := sendMessage.Call(dialog.fallback, 0x00f0, 0, 0)
	if err := controller.savePort(windows.UTF16ToString(text[:]), checked == 1); err != nil {
		setWindowText.Call(dialog.errorLabel, uintptr(unsafe.Pointer(utf16(err.Error()))))
		return
	}
	w.closeSettings()
}

func (w *windowsUI) closeSettings() {
	dialog := w.dialog
	w.dialog = nil
	destroyWindow.Call(dialog.hwnd)
	if dialog.font != 0 {
		gdi32.NewProc("DeleteObject").Call(dialog.font)
	}
	if w.standalone {
		destroyWindow.Call(w.hwnd)
	}
}

func clipboardText(hwnd uintptr, text string) error {
	opened, _, err := user32.NewProc("OpenClipboard").Call(hwnd)
	if opened == 0 {
		return fmt.Errorf("无法打开剪贴板：%v", err)
	}
	defer user32.NewProc("CloseClipboard").Call()
	encoded := windows.StringToUTF16(text)
	handle, _, err := kernel32.NewProc("GlobalAlloc").Call(0x42, uintptr(len(encoded)*2))
	if handle == 0 {
		return err
	}
	memory, _, err := kernel32.NewProc("GlobalLock").Call(handle)
	if memory == 0 {
		kernel32.NewProc("GlobalFree").Call(handle)
		return err
	}
	moveMemory.Call(memory, uintptr(unsafe.Pointer(&encoded[0])), uintptr(len(encoded)*2))
	kernel32.NewProc("GlobalUnlock").Call(handle)
	user32.NewProc("EmptyClipboard").Call()
	result, _, err := user32.NewProc("SetClipboardData").Call(13, handle)
	if result == 0 {
		kernel32.NewProc("GlobalFree").Call(handle)
		return err
	}
	return nil // 成功后内存归系统剪贴板所有。
}

func archiveIcon() uintptr {
	// 用透明背景的蓝色档案盒绘制 32px 图标，避免依赖安装目录和额外资源文件。
	var mask [128]byte
	var pixels [32 * 32 * 4]byte
	for i := range mask {
		mask[i] = 0xff
	}
	for y := 5; y < 27; y++ {
		for x := 4; x < 28; x++ {
			ink := y >= 5 && y <= 9 || y >= 11 && y <= 26 && (x <= 6 || x >= 25 || y >= 24) || y >= 14 && y <= 16 && x >= 12 && x <= 19
			if !ink {
				continue
			}
			offset := (y*32 + x) * 4
			pixels[offset], pixels[offset+1], pixels[offset+2], pixels[offset+3] = 235, 120, 45, 255
			mask[y*4+x/8] &^= 1 << uint(7-x%8)
		}
	}
	icon, _, _ := user32.NewProc("CreateIcon").Call(0, 32, 32, 1, 32, uintptr(unsafe.Pointer(&mask[0])), uintptr(unsafe.Pointer(&pixels[0])))
	return icon
}
