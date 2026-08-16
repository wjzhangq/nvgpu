//go:build !no_nvml

package collector

import (
	"context"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/wjzhangq/gpumon/internal/model"
)

// collectGPUsNVML 使用 NVML 库直接查询 GPU（本地）。
// 相比 nvidia-smi，NVML 避免了进程启动开销，速度快 4-5 倍（5-15ms vs 50-200ms）。
func (l *Local) collectGPUsNVML(ctx context.Context) []model.GPU {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil // fallback 到 nvidia-smi
	}
	defer nvml.Shutdown()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS || count == 0 {
		return nil
	}

	gpus := make([]model.GPU, 0, count)
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			continue
		}

		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			name = ""
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			uuid = ""
		}

		util, ret := device.GetUtilizationRates()
		var gpuUtil uint32
		if ret == nvml.SUCCESS {
			gpuUtil = util.Gpu
		}

		mem, ret := device.GetMemoryInfo()
		var memTotal, memUsed uint64
		if ret == nvml.SUCCESS {
			memTotal = mem.Total
			memUsed = mem.Used
		}

		// 温度（℃）。部分虚拟化/容器环境不支持，失败时留 0。
		temp, ret := device.GetTemperature(nvml.TEMPERATURE_GPU)
		var tempValue uint32
		if ret == nvml.SUCCESS {
			tempValue = temp
		}

		// 功耗。NVML 返回毫瓦，转换为瓦特。
		power, ret := device.GetPowerUsage()
		var powerValue float64
		if ret == nvml.SUCCESS {
			powerValue = round2(float64(power) / 1000.0)
		}

		gpus = append(gpus, model.GPU{
			Index:              i,
			Model:              name,
			UUID:               uuid,
			UtilizationPercent: round2(float64(gpuUtil)),
			VRAMTotalBytes:     memTotal,
			VRAMUsedBytes:      memUsed,
			VRAMUsagePercent:   round2(model.Percent(memUsed, memTotal)),
			TemperatureCelsius: tempValue,
			PowerWatts:         powerValue,
		})
	}
	return gpus
}
