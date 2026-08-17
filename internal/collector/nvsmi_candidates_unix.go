//go:build !windows

package collector

// nvidiaSmiCandidates 在非 Windows 平台上返回固定列表（PATH + 几个常见目录）。
func nvidiaSmiCandidates() []pathCandidate {
	out := []pathCandidate{
		{Path: "nvidia-smi", Source: "PATH"},
	}
	for _, p := range []string{
		"/usr/bin/nvidia-smi",
		"/usr/local/bin/nvidia-smi",
		"/usr/local/nvidia/bin/nvidia-smi",
		"/opt/nvidia/bin/nvidia-smi",
	} {
		c := pathCandidate{Path: p, Source: "固定位置"}
		if !isFile(p) {
			c.Miss = "不存在"
		}
		out = append(out, c)
	}
	return out
}
