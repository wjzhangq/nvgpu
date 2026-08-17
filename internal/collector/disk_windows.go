//go:build windows

package collector

import (
	"context"
	"fmt"
	"syscall"

	"github.com/shirou/gopsutil/v4/disk"
	"github.com/wjzhangq/gpumon/internal/logx"
	"golang.org/x/sys/windows"
)

// listPartitions 返回 Windows 磁盘分区列表。
// 只返回 DRIVE_FIXED 类型（可移动盘/光驱/网络盘跳过），除非在白名单显式列出。
// 盘符统一上报为 "C:\" 形式。
func listPartitions(ctx context.Context, whitelist map[string]bool) ([]disk.PartitionStat, error) {
	bitMask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("GetLogicalDrives: %w", err)
	}

	var parts []disk.PartitionStat

	for i := 0; i < 26; i++ {
		if bitMask&(1<<uint(i)) == 0 {
			continue // 盘符不存在
		}

		letter := string(rune('A' + i))
		root := letter + ":\\"
		rootPtr, err := syscall.UTF16PtrFromString(root)
		if err != nil {
			continue
		}

		driveType := windows.GetDriveType(rootPtr)

		// 只采 DRIVE_FIXED，除非白名单显式列出
		skip := false
		reason := ""
		if driveType != windows.DRIVE_FIXED {
			if !whitelist[normalizeMountKey(root)] {
				skip = true
				reason = driveTypeString(driveType)
			}
		}

		if logx.Verbose() {
			if skip {
				logx.Debugf("跳过盘符 %s (type=%s)", root, reason)
			} else {
				logx.Debugf("采集盘符 %s (type=%s)", root, driveTypeString(driveType))
			}
		}

		if skip {
			continue
		}

		// 查询文件系统类型
		var volNameBuf [256]uint16
		var fsNameBuf [256]uint16
		err = windows.GetVolumeInformation(
			rootPtr,
			&volNameBuf[0], uint32(len(volNameBuf)),
			nil, nil, nil,
			&fsNameBuf[0], uint32(len(fsNameBuf)),
		)

		fstype := "unknown"
		if err == nil {
			fstype = syscall.UTF16ToString(fsNameBuf[:])
		}

		parts = append(parts, disk.PartitionStat{
			Device:     root,
			Mountpoint: root,
			Fstype:     fstype,
		})
	}

	return parts, nil
}

// diskUsage 查询 Windows 磁盘用量。
func diskUsage(ctx context.Context, path string) (*disk.UsageStat, error) {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	err = windows.GetDiskFreeSpaceEx(pathPtr, &freeBytesAvailable, &totalBytes, &totalFreeBytes)
	if err != nil {
		return nil, fmt.Errorf("GetDiskFreeSpaceEx(%s): %w", path, err)
	}

	usedBytes := totalBytes - totalFreeBytes
	usedPercent := 0.0
	if totalBytes > 0 {
		usedPercent = float64(usedBytes) * 100.0 / float64(totalBytes)
	}

	return &disk.UsageStat{
		Path:        path,
		Total:       totalBytes,
		Free:        totalFreeBytes,
		Used:        usedBytes,
		UsedPercent: usedPercent,
	}, nil
}

// driveTypeString 返回 Windows 驱动器类型的字符串表示。
func driveTypeString(t uint32) string {
	switch t {
	case windows.DRIVE_REMOVABLE:
		return "removable"
	case windows.DRIVE_FIXED:
		return "fixed"
	case windows.DRIVE_REMOTE:
		return "remote"
	case windows.DRIVE_CDROM:
		return "cdrom"
	case windows.DRIVE_RAMDISK:
		return "ramdisk"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}
