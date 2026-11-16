// 中文说明：在 Windows 后台启动服务，避免双击启动器时弹出额外控制台窗口。
package desktop

import (
	"golang.org/x/sys/windows"
	"os/exec"
	"syscall"
)

func detach(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS, HideWindow: true}
}
