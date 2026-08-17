//go:build !windows

package collector

import (
	"context"

	"github.com/wjzhangq/gpumon/internal/model"
)

// collectBlockDevicesWindows 在非 Windows 平台不可用。
// 保留同名空实现，让 local.go 里的 runtime.GOOS 分支不必带构建标签。
func collectBlockDevicesWindows(ctx context.Context, whitelist map[string]bool) ([]model.Disk, error) {
	return nil, nil
}
