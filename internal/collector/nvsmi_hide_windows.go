//go:build windows

package collector

import (
	"os/exec"
	"syscall"
)

// hideWindow 阻止子进程弹出控制台窗口。
//
// nvgpu 作为 Windows 服务运行时，每个采集周期都要 fork 一次 nvidia-smi.exe；
// 不设 CREATE_NO_WINDOW 的话，交互式会话下会看到黑框一闪一闪。
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
}
