//go:build !windows

package collector

import (
	"os"
	"os/exec"
)

// defaultNvidiaSmiPath 在 Linux 上定位 nvidia-smi。
func defaultNvidiaSmiPath() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	// systemd 拉起的服务 PATH 往往很窄，补几个常见位置。
	for _, p := range []string{
		"/usr/bin/nvidia-smi",
		"/usr/local/bin/nvidia-smi",
		"/usr/local/nvidia/bin/nvidia-smi",
		"/opt/nvidia/bin/nvidia-smi",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
