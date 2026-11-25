// 中文说明：在原生界面线程启用 Windows 公共控件的系统视觉样式；运行时清单随窗口退出清理，无需新增构建工具。
package desktopui

import (
	"os"
	"unsafe"
)

type activationContext struct {
	Size, Flags                      uint32
	Source                           *uint16
	Architecture, Language           uint16
	Directory, Resource, Application *uint16
	Module                           uintptr
}

func activateVisualStyles() func() {
	const manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0"><assemblyIdentity type="win32" name="CodexChronicle.Desktop" version="1.0.0.0" processorArchitecture="*"/><dependency><dependentAssembly><assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/></dependentAssembly></dependency></assembly>`
	noop := func() {}
	file, err := os.CreateTemp("", "chronicle-controls-*.manifest")
	if err != nil {
		return noop
	}
	_, writeErr := file.WriteString(manifest)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(file.Name())
		return noop
	}
	context := activationContext{Source: utf16(file.Name())}
	context.Size = uint32(unsafe.Sizeof(context))
	handle, _, _ := kernel32.NewProc("CreateActCtxW").Call(uintptr(unsafe.Pointer(&context)))
	if handle == ^uintptr(0) {
		os.Remove(file.Name())
		return noop
	}
	var cookie uintptr
	activated, _, _ := kernel32.NewProc("ActivateActCtx").Call(handle, uintptr(unsafe.Pointer(&cookie)))
	if activated == 0 {
		kernel32.NewProc("ReleaseActCtx").Call(handle)
		os.Remove(file.Name())
		return noop
	}
	return func() {
		kernel32.NewProc("DeactivateActCtx").Call(0, cookie)
		kernel32.NewProc("ReleaseActCtx").Call(handle)
		os.Remove(file.Name())
	}
}
