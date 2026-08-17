//go:build windows

package collector

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// nvidiaSmiCandidates 列出 Windows 上所有会被检查的 nvidia-smi 位置，
// 按检查顺序返回。诊断命令用它展示"查了哪些地方、哪个命中了"。
//
// 探测顺序（从最可靠到最兜底）：
//  1. PATH —— 装了 CUDA Toolkit 或手工配过环境变量时命中。
//  2. 固定安装位置 —— NVSMI 老目录、System32、以及 Program Files 下的
//     NVIDIA app / Corporation 子目录。
//  3. DriverStore FileRepository —— 新驱动（R470+）把 nvidia-smi.exe 塞进
//     nv_dispi.inf_amd64_* / nvdm*.inf_amd64_* 这类版本化目录里，System32
//     下只留一个转发用的 DLL。这里按 glob 扫，并挑修改时间最新的一个，
//     避免命中残留的旧驱动副本。
//
// 注意 Windows 服务运行时的环境差异：服务进程默认继承的是系统 PATH 而不是
// 登录用户的 PATH，所以第 1 步在服务里经常扑空，第 2、3 步才是主力。
func nvidiaSmiCandidates() []pathCandidate {
	var out []pathCandidate

	for _, name := range []string{"nvidia-smi.exe", "nvidia-smi"} {
		if p, err := exec.LookPath(name); err == nil {
			out = append(out, pathCandidate{Path: p, Source: "PATH"})
		} else {
			out = append(out, pathCandidate{Path: name, Source: "PATH", Miss: err.Error()})
		}
	}

	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		systemRoot = `C:\Windows`
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	programW6432 := os.Getenv("ProgramW6432")

	fixed := []string{
		filepath.Join(programFiles, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"),
		filepath.Join(programFiles, "NVIDIA Corporation", "NVIDIA app", "nvidia-smi.exe"),
		filepath.Join(programFiles, "NVIDIA GPU Computing Toolkit", "CUDA", "bin", "nvidia-smi.exe"),
		filepath.Join(systemRoot, "System32", "nvidia-smi.exe"),
	}
	if programW6432 != "" && programW6432 != programFiles {
		fixed = append(fixed,
			filepath.Join(programW6432, "NVIDIA Corporation", "NVSMI", "nvidia-smi.exe"),
			filepath.Join(programW6432, "NVIDIA Corporation", "NVIDIA app", "nvidia-smi.exe"),
		)
	}
	for _, p := range fixed {
		c := pathCandidate{Path: p, Source: "固定位置"}
		if !isFile(p) {
			c.Miss = "不存在"
		}
		out = append(out, c)
	}

	for _, m := range driverStoreMatches(systemRoot) {
		out = append(out, pathCandidate{Path: m.path, Source: "DriverStore"})
	}
	return out
}

// driverStoreCandidate 是 DriverStore 里的一个 nvidia-smi.exe 副本。
type driverStoreCandidate struct {
	path string
	mod  int64
}

// driverStoreMatches 扫 DriverStore 下所有 nvidia-smi.exe，最新的排在前面。
// 目录名带驱动版本号，升级后旧副本常常还留在盘上，所以要按 mtime 取最新。
func driverStoreMatches(systemRoot string) []driverStoreCandidate {
	repo := filepath.Join(systemRoot, "System32", "DriverStore", "FileRepository")
	patterns := []string{"nv_dispi.inf_*", "nvdm*", "nv_*"}

	var found []driverStoreCandidate
	seen := make(map[string]bool)

	for _, pat := range patterns {
		matches, err := filepath.Glob(filepath.Join(repo, pat, "nvidia-smi.exe"))
		if err != nil {
			continue
		}
		for _, m := range matches {
			if seen[m] {
				continue
			}
			st, err := os.Stat(m)
			if err != nil || st.IsDir() {
				continue
			}
			seen[m] = true
			found = append(found, driverStoreCandidate{path: m, mod: st.ModTime().UnixNano()})
		}
	}

	// 最新的驱动副本优先；时间相同时按路径稳定排序。
	sort.Slice(found, func(i, j int) bool {
		if found[i].mod != found[j].mod {
			return found[i].mod > found[j].mod
		}
		return found[i].path < found[j].path
	})
	return found
}

// defaultNvidiaSmiPath 在 Windows 上定位 nvidia-smi.exe。
// 返回 nvidiaSmiCandidates 里第一个真实存在的路径。
func defaultNvidiaSmiPath() string {
	for _, c := range nvidiaSmiCandidates() {
		if c.Miss == "" && isFile(c.Path) {
			return c.Path
		}
	}
	return ""
}
