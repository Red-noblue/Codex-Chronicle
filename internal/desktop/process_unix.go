//go:build !windows

// 中文说明：将安装版后台与启动终端分离，关闭浏览器或终端不会强制中断数据任务。
package desktop

import (
	"os/exec"
	"syscall"
)

func detach(command *exec.Cmd) { command.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
