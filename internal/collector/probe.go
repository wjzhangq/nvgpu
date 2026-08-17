package collector

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
)

// ProbeGPU 打印一份 GPU 采集诊断报告：探测到的 nvidia-smi 路径、实际执行的
// 命令行、退出状态、原始输出，以及解析结果。
//
// 存在的理由是 Windows：nvgpu 以服务方式运行时 stdout 进不了任何人看得见的
// 地方，"看板上 GPU 是空的" 之外没有别的线索。这个子命令让用户能在交互式
// 终端里一步定位是路径没找到、驱动没起来，还是输出格式没对上。
func ProbeGPU(ctx context.Context, w io.Writer, configuredPath string) {
	fmt.Fprintf(w, "平台: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "NVML 支持: %s\n", nvmlBuildStatus())

	path := strings.TrimSpace(configuredPath)
	switch {
	case path != "":
		fmt.Fprintf(w, "nvidia-smi: %s（来自配置 nvidia_smi）\n", path)
		if !isFile(path) {
			fmt.Fprintf(w, "  !! 该路径不存在或不是文件\n")
		}
	default:
		path = defaultNvidiaSmiPath()
		if path == "" {
			fmt.Fprintf(w, "nvidia-smi: 未找到\n\n")
			fmt.Fprintf(w, "排查建议:\n")
			fmt.Fprintf(w, "  1. 在终端直接执行 nvidia-smi 确认驱动已安装\n")
			fmt.Fprintf(w, "  2. 用 where nvidia-smi (Windows) / which nvidia-smi (Linux) 找到真实路径\n")
			fmt.Fprintf(w, "  3. 把该路径写进配置的 nvidia_smi 字段\n")
			return
		}
		fmt.Fprintf(w, "nvidia-smi: %s（自动探测）\n", path)
	}

	args := nvidiaArgs()
	fmt.Fprintf(w, "执行: %s %s\n\n", path, strings.Join(args, " "))

	res := runNvidiaSmi(ctx, path, args)
	if res.err != nil {
		fmt.Fprintf(w, "退出状态: %v\n", res.err)
	} else {
		fmt.Fprintf(w, "退出状态: 成功\n")
	}
	if res.stderr != "" {
		fmt.Fprintf(w, "stderr:\n%s\n", indent(res.stderr))
	}
	if strings.TrimSpace(res.stdout) != "" {
		fmt.Fprintf(w, "stdout:\n%s\n", indent(res.stdout))
	} else {
		fmt.Fprintf(w, "stdout: (空)\n")
	}

	gpus := parseNvidiaCSV(res.stdout)
	fmt.Fprintf(w, "\n解析出 %d 张 GPU\n", len(gpus))
	for _, g := range gpus {
		fmt.Fprintf(w, "  [%d] %s  利用率 %.0f%%  显存 %d/%d MiB  %d℃  %.1fW\n",
			g.Index, g.Model, g.UtilizationPercent,
			g.VRAMUsedBytes/mib, g.VRAMTotalBytes/mib,
			g.TemperatureCelsius, g.PowerWatts)
	}
	if len(gpus) == 0 {
		fmt.Fprintf(w, "\n输出无法解析成 GPU 记录。期望的字段顺序:\n  %s\n", nvidiaQuery)
	}
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + strings.TrimSuffix(l, "\r")
	}
	return strings.Join(lines, "\n")
}
