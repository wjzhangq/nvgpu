package collector

import (
	"context"
	"os/exec"
	"testing"

	"github.com/wjzhangq/gpumon/internal/model"
)

// BenchmarkCollectGPUsNvidiaSmi 测试 nvidia-smi 方式的 GPU 采集性能。
func BenchmarkCollectGPUsNvidiaSmi(b *testing.B) {
	l := &Local{nvsmi: "nvidia-smi"}
	ctx := context.Background()

	// 预热：确保 nvidia-smi 路径正确
	gpus := l.collectGPUsViaSmi(ctx)
	if len(gpus) == 0 {
		b.Skip("未检测到 GPU 或 nvidia-smi 不可用")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.collectGPUsViaSmi(ctx)
	}
}

// BenchmarkCollectGPUsNVML 测试 NVML 方式的 GPU 采集性能。
func BenchmarkCollectGPUsNVML(b *testing.B) {
	l := &Local{}
	ctx := context.Background()

	// 预热：确保 NVML 可用
	gpus := l.collectGPUsNVML(ctx)
	if len(gpus) == 0 {
		b.Skip("未检测到 GPU 或 NVML 不可用")
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.collectGPUsNVML(ctx)
	}
}

// collectGPUsViaSmi 是 collectGPUs 的 nvidia-smi 专用版本，用于基准测试对比。
func (l *Local) collectGPUsViaSmi(ctx context.Context) []model.GPU {
	if l.nvsmi == "" {
		l.nvsmi = defaultNvidiaSmiPath()
	}
	if l.nvsmi == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, l.nvsmi, nvidiaArgs()...)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNvidiaCSV(string(out))
}
