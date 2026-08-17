//go:build !windows

package collector

import "os/exec"

// hideWindow 在非 Windows 平台上无事可做。
func hideWindow(cmd *exec.Cmd) {}
