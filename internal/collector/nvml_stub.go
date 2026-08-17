//go:build no_nvml

package collector

import (
	"context"

	"github.com/wjzhangq/gpumon/internal/model"
)

// nvmlBuildStatus 供诊断输出使用，说明本二进制是否编入了 NVML。
func nvmlBuildStatus() string { return "未编入（构建标签 no_nvml，仅使用 nvidia-smi）" }

// collectGPUsNVML 在 no_nvml 构建标签下是一个空实现，始终返回 nil。
// 这允许在不支持 CGO 的环境下编译纯 Go 版本，自动回退到 nvidia-smi。
func (l *Local) collectGPUsNVML(ctx context.Context) []model.GPU {
	return nil
}
