package collector

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/wjzhangq/gpumon/internal/model"
)

// ProbeGPU 打印一份 GPU 采集诊断报告。
//
// 存在的理由是 Windows：nvgpu 以服务方式运行时 stdout 进不了任何人看得见的
// 地方，"看板上 GPU 是空的" 之外没有别的线索。报告要能一次性回答这几个问题：
//   - 这个二进制有没有编入 NVML？NVML 这条路走通了吗？
//   - nvidia-smi 找过哪些位置？分别为什么没命中？最后用的是哪个？
//   - 驱动版本是多少？（能区分"驱动没装"和"装了但查不到卡"）
//   - 实际执行了什么命令，原样输出是什么，解析成了几张卡？
//
// configuredPath 非空时跳过自动探测，直接用它 —— 对应配置里的 nvidia_smi
// 字段或命令行传入的路径。
func ProbeGPU(ctx context.Context, w io.Writer, configuredPath string) {
	section(w, "环境")
	fmt.Fprintf(w, "  平台:      %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "  NVML 支持: %s\n", nvmlBuildStatus())
	if runtime.GOOS == "windows" {
		// 服务进程继承的是系统 PATH，和登录用户的不一样，这行经常是关键线索。
		fmt.Fprintf(w, "  PATH:      %s\n", truncate(os.Getenv("PATH"), 300))
	}

	probeNVML(ctx, w)

	path := resolveProbePath(w, configuredPath)
	if path == "" {
		return
	}

	probeDriverVersion(ctx, w, path)
	probeCollect(ctx, w, path)
}

// probeNVML 报告 NVML 这条路的结果。
// 它是本机采集的首选路径，走通了就完全不碰 nvidia-smi。
func probeNVML(ctx context.Context, w io.Writer) {
	section(w, "NVML")
	l := &Local{name: "probe"}
	gpus := l.collectGPUsNVML(ctx)
	if len(gpus) == 0 {
		fmt.Fprintf(w, "  未取到数据（将回退 nvidia-smi）\n")
		return
	}
	fmt.Fprintf(w, "  成功取到 %d 张 GPU —— 实际运行时会走这条路，不调用 nvidia-smi\n", len(gpus))
	for _, g := range gpus {
		fmt.Fprintf(w, "%s\n", formatGPU(g))
	}
}

// resolveProbePath 决定用哪个 nvidia-smi，并把探测过程打出来。
// 返回空串表示没有可用路径，调用方应终止诊断。
func resolveProbePath(w io.Writer, configuredPath string) string {
	section(w, "nvidia-smi 路径")

	if p := strings.TrimSpace(configuredPath); p != "" {
		fmt.Fprintf(w, "  指定路径: %s\n", p)
		if !isFile(p) {
			fmt.Fprintf(w, "  !! 不存在或不是文件\n")
			hintNotFound(w)
			return ""
		}
		fmt.Fprintf(w, "  存在 ✓\n")
		return p
	}

	fmt.Fprintf(w, "  自动探测，按顺序检查:\n")
	var chosen string
	for _, c := range nvidiaSmiCandidates() {
		switch {
		case c.Miss != "":
			fmt.Fprintf(w, "    ✗ [%s] %s —— %s\n", c.Source, c.Path, c.Miss)
		case !isFile(c.Path):
			fmt.Fprintf(w, "    ✗ [%s] %s —— 不存在\n", c.Source, c.Path)
		case chosen == "":
			chosen = c.Path
			fmt.Fprintf(w, "    ✓ [%s] %s  ← 采用\n", c.Source, c.Path)
		default:
			fmt.Fprintf(w, "    · [%s] %s（存在，未采用）\n", c.Source, c.Path)
		}
	}

	if chosen == "" {
		fmt.Fprintf(w, "\n  所有位置均未命中\n")
		hintNotFound(w)
		return ""
	}
	return chosen
}

// probeDriverVersion 单独查一次驱动版本。
// 这条查询比 --query-gpu 全字段更宽容，能把"驱动没装/没起来"和
// "驱动正常但字段对不上"区分开。
func probeDriverVersion(ctx context.Context, w io.Writer, path string) {
	section(w, "驱动")
	args := nvidiaSmiVersionArgs()
	res := runNvidiaSmi(ctx, path, args)
	if res.err != nil {
		fmt.Fprintf(w, "  查询失败: %s\n", res.briefError())
		return
	}
	out := strings.TrimSpace(res.stdout)
	if out == "" {
		fmt.Fprintf(w, "  命令成功但无输出 —— 可能没有可见的 NVIDIA 卡\n")
		return
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(strings.TrimSuffix(line, "\r")); line != "" {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}

// probeCollect 执行真实采集用的那条命令，并展示原始输出与解析结果。
func probeCollect(ctx context.Context, w io.Writer, path string) {
	section(w, "采集")
	args := nvidiaArgs()
	fmt.Fprintf(w, "  命令: %s %s\n\n", path, strings.Join(args, " "))

	res := runNvidiaSmi(ctx, path, args)
	if res.err != nil {
		fmt.Fprintf(w, "  退出状态: %v\n", res.err)
	} else {
		fmt.Fprintf(w, "  退出状态: 成功\n")
	}
	if res.stderr != "" {
		fmt.Fprintf(w, "  stderr:\n%s\n", indent(res.stderr, "    "))
	}
	if strings.TrimSpace(res.stdout) != "" {
		fmt.Fprintf(w, "  stdout:\n%s\n", indent(res.stdout, "    "))
	} else {
		fmt.Fprintf(w, "  stdout: (空)\n")
	}

	gpus := parseNvidiaCSV(res.stdout)
	fmt.Fprintf(w, "\n  解析结果: %d 张 GPU\n", len(gpus))
	for _, g := range gpus {
		fmt.Fprintf(w, "%s\n", formatGPU(g))
	}
	if len(gpus) == 0 {
		fmt.Fprintf(w, "\n  输出无法解析。期望 8 个字段，顺序为:\n    %s\n", nvidiaQuery)
	}
}

// formatGPU 把一张卡渲染成两行：标识 + 指标。
func formatGPU(g model.GPU) string {
	var b strings.Builder
	fmt.Fprintf(&b, "    [%d] %s\n", g.Index, g.Model)
	fmt.Fprintf(&b, "        利用率 %.0f%%  显存 %d/%d MiB (%.1f%%)",
		g.UtilizationPercent, g.VRAMUsedBytes/mib, g.VRAMTotalBytes/mib, g.VRAMUsagePercent)
	if g.TemperatureCelsius > 0 {
		fmt.Fprintf(&b, "  %d℃", g.TemperatureCelsius)
	}
	if g.PowerWatts > 0 {
		fmt.Fprintf(&b, "  %.1fW", g.PowerWatts)
	}
	if g.UUID != "" {
		fmt.Fprintf(&b, "\n        UUID %s", g.UUID)
	}
	return b.String()
}

func hintNotFound(w io.Writer) {
	fmt.Fprintf(w, "\n排查建议:\n")
	if runtime.GOOS == "windows" {
		fmt.Fprintf(w, "  1. 在 PowerShell 里执行 nvidia-smi.exe 确认驱动已安装\n")
		fmt.Fprintf(w, "  2. 执行 where.exe nvidia-smi.exe 找到真实路径\n")
		fmt.Fprintf(w, "  3. 用该路径重跑诊断: nvgpu.exe gpu-probe \"<路径>\"\n")
		fmt.Fprintf(w, "  4. 确认无误后写进配置的 nvidia_smi 字段并重启服务\n")
		return
	}
	fmt.Fprintf(w, "  1. 在终端执行 nvidia-smi 确认驱动已安装\n")
	fmt.Fprintf(w, "  2. 执行 which nvidia-smi 找到真实路径\n")
	fmt.Fprintf(w, "  3. 用该路径重跑诊断: nvgpu gpu-probe <路径>\n")
	fmt.Fprintf(w, "  4. 确认无误后写进配置的 nvidia_smi 字段\n")
}

func section(w io.Writer, title string) {
	fmt.Fprintf(w, "\n== %s ==\n", title)
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + strings.TrimSuffix(l, "\r")
	}
	return strings.Join(lines, "\n")
}
