//go:build !windows

package collector

import (
	"context"

	"github.com/shirou/gopsutil/v4/disk"
)

// listPartitions 返回文件系统挂载点列表（非 Windows）。
// whitelist 在这里用不上 —— gopsutil 在 Unix 上不会返回会阻塞的挂载点，
// 过滤交给上层的 isPseudoFS / diskFilter 处理，保持原有行为。
func listPartitions(ctx context.Context, whitelist map[string]bool) ([]disk.PartitionStat, error) {
	return disk.PartitionsWithContext(ctx, false)
}

// diskUsage 查询挂载点用量（非 Windows）。
func diskUsage(ctx context.Context, path string) (*disk.UsageStat, error) {
	return disk.UsageWithContext(ctx, path)
}
