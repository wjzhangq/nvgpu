//go:build windows

package collector

import (
	"os"
	"os/exec"
	"path/filepath"
)

// defaultNvidiaSmiPath 在 Windows 上定位 nvidia-smi.exe。
//
// 新驱动不再往 System32 放 NVSMI，可执行文件躲在 DriverStore 的
// nvdm*.inf_amd64_* 目录里，所以要额外做一次 glob（沿用 gpu2 的做法）。
func defaultNvidiaSmiPath() string {
	for _, name := range []string{"nvidia-smi.exe", "nvidia-smi"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}

	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}

	fixed := []string{
		`C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`,
		filepath.Join(systemRoot, "System32", "nvidia-smi.exe"),
	}
	for _, p := range fixed {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}

	pattern := filepath.Join(systemRoot, "System32", "DriverStore", "FileRepository", "nvdm*", "nvidia-smi.exe")
	if matches, err := filepath.Glob(pattern); err == nil {
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && !st.IsDir() {
				return m
			}
		}
	}
	return ""
}
