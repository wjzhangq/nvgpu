//go:build !no_nvml

package collector

import (
	"context"
	"testing"
)

func TestCollectGPUsNVML(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 NVIDIA GPU 和驱动")
	}

	l := &Local{}
	gpus := l.collectGPUsNVML(context.Background())

	if len(gpus) == 0 {
		t.Skip("未检测到 GPU（可能无 NVIDIA 硬件或驱动未安装）")
	}

	// 验证第一张卡的基本数据完整性
	g := gpus[0]
	if g.Model == "" {
		t.Errorf("GPU 型号为空")
	}
	if g.VRAMTotalBytes == 0 {
		t.Errorf("显存总量为 0")
	}
	if g.Index != 0 {
		t.Errorf("第一张卡 Index 应该是 0，得到 %d", g.Index)
	}

	t.Logf("检测到 %d 张 GPU", len(gpus))
	for i, gpu := range gpus {
		t.Logf("GPU[%d]: %s, VRAM: %d MB, 利用率: %.1f%%, 显存占用: %.1f%%",
			i, gpu.Model, gpu.VRAMTotalBytes/1024/1024,
			gpu.UtilizationPercent, gpu.VRAMUsagePercent)
	}
}
