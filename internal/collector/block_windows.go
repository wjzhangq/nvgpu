//go:build windows

package collector

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/wjzhangq/gpumon/internal/logx"
	"github.com/wjzhangq/gpumon/internal/model"
	"golang.org/x/sys/windows"
)

const (
	ioctlDiskGetDriveGeometryEx = 0x000700A0
	ioctlStorageQueryProperty   = 0x002D1400

	storageDeviceProperty            = 0
	storageDeviceSeekPenaltyProperty = 7
	propertyStandardQuery            = 0

	maxPhysicalDrives = 64
)

// collectBlockDevicesWindows 枚举 Windows 物理磁盘 \\.\PHYSICALDRIVE0..63，
// 用原生 DeviceIoControl 查询容量、型号、可移动标记、SSD 标记。
//
// 替换原来的 wmic / PowerShell 子进程方案，修掉三个问题：
//  1. wmic CSV 按字母序输出列（Node,DeviceID,MediaType,Model,Size），
//     旧代码按请求顺序取 f[3] 当 Size，实际拿到 Model，parseUint 返回 0，
//     每块盘都被 minBlockDeviceBytes 过滤掉 —— 这就是"Windows 获取磁盘有问题"。
//  2. wmic 在 Win11 24H2 / Server 2025 已被移除。
//  3. 子进程未设 hideWindow，服务模式下会闪黑框。
func collectBlockDevicesWindows(ctx context.Context, whitelist map[string]bool) ([]model.Disk, error) {
	var devices []model.Disk

	for i := 0; i < maxPhysicalDrives; i++ {
		// DeviceIoControl 本身不接受 ctx，只能在每块盘之间检查取消。
		// 单块盘的元数据查询是毫秒级的，这个粒度足够。
		if err := ctx.Err(); err != nil {
			if logx.Verbose() {
				logx.Debugf("物理磁盘枚举在第 %d 块处中止: %v", i, err)
			}
			return devices, nil
		}
		name := fmt.Sprintf("PhysicalDrive%d", i)
		dev, ok := probePhysicalDrive(i, name, whitelist)
		if ok {
			devices = append(devices, dev)
		}
	}

	if logx.Verbose() {
		logx.Debugf("Windows 物理磁盘枚举完成，共 %d 块", len(devices))
	}
	return devices, nil
}

// probePhysicalDrive 探测单块物理磁盘，返回 (磁盘信息, 是否应上报)。
func probePhysicalDrive(index int, name string, whitelist map[string]bool) (model.Disk, bool) {
	var dev model.Disk

	path := fmt.Sprintf(`\\.\PHYSICALDRIVE%d`, index)
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return dev, false
	}

	// dwDesiredAccess=0：纯元数据查询，不需要管理员权限，也不唤醒介质。
	handle, err := windows.CreateFile(
		pathPtr,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		// 磁盘号不存在（热插拔后编号可能不连续），静默跳过
		if err != windows.ERROR_FILE_NOT_FOUND && logx.Verbose() {
			logx.Debugf("打开 %s 失败: %v", path, err)
		}
		return dev, false
	}
	defer windows.CloseHandle(handle)

	inWhitelist := whitelist[normalizeBlockKey(name)]

	dev.Device = name
	dev.Mount = name
	dev.Type = "disk"

	// 容量
	if size, err := ioctlDiskSize(handle); err == nil {
		dev.TotalBytes = size
	} else if logx.Verbose() {
		logx.Debugf("%s: 查询容量失败: %v", name, err)
	}

	// 型号 + 可移动标记
	if modelStr, removable, err := ioctlDeviceDescriptor(handle); err == nil {
		dev.Model = modelStr
		// 可移动介质（光驱、读卡器）默认跳过，白名单显式列出时保留
		if removable && !inWhitelist {
			if logx.Verbose() {
				logx.Debugf("跳过可移动磁盘 %s (%s)", name, modelStr)
			}
			return dev, false
		}
	} else if logx.Verbose() {
		logx.Debugf("%s: 查询设备描述符失败: %v", name, err)
	}

	// 机械盘 / SSD
	if rotational, err := ioctlSeekPenalty(handle); err == nil {
		dev.Rotational = &rotational
	}

	// 容量下限过滤，白名单显式列出时不过滤
	if dev.TotalBytes < minBlockDeviceBytes && !inWhitelist {
		if logx.Verbose() {
			logx.Debugf("跳过小容量磁盘 %s (%d bytes < %d)",
				name, dev.TotalBytes, minBlockDeviceBytes)
		}
		return dev, false
	}

	if logx.Verbose() {
		logx.Debugf("采集磁盘 %s model=%q size=%d bytes", name, dev.Model, dev.TotalBytes)
	}
	return dev, true
}

// ioctlDiskSize 发 IOCTL_DISK_GET_DRIVE_GEOMETRY_EX 查询磁盘容量。
func ioctlDiskSize(handle windows.Handle) (uint64, error) {
	var buf [256]byte
	var n uint32

	err := windows.DeviceIoControl(
		handle, ioctlDiskGetDriveGeometryEx,
		nil, 0,
		&buf[0], uint32(len(buf)),
		&n, nil,
	)
	if err != nil {
		return 0, err
	}
	return parseDiskGeometryEx(buf[:], n)
}

// ioctlDeviceDescriptor 发 IOCTL_STORAGE_QUERY_PROPERTY + StorageDeviceProperty
// 查询型号与可移动标记。
func ioctlDeviceDescriptor(handle windows.Handle) (string, bool, error) {
	query := storagePropertyQuery(storageDeviceProperty)

	var buf [1024]byte
	var n uint32

	err := windows.DeviceIoControl(
		handle, ioctlStorageQueryProperty,
		&query[0], uint32(len(query)),
		&buf[0], uint32(len(buf)),
		&n, nil,
	)
	if err != nil {
		return "", false, err
	}
	return parseStorageDeviceDescriptor(buf[:], n)
}

// ioctlSeekPenalty 发 IOCTL_STORAGE_QUERY_PROPERTY +
// StorageDeviceSeekPenaltyProperty 判断机械盘 / SSD。
func ioctlSeekPenalty(handle windows.Handle) (bool, error) {
	query := storagePropertyQuery(storageDeviceSeekPenaltyProperty)

	var buf [64]byte
	var n uint32

	err := windows.DeviceIoControl(
		handle, ioctlStorageQueryProperty,
		&query[0], uint32(len(query)),
		&buf[0], uint32(len(buf)),
		&n, nil,
	)
	if err != nil {
		return false, err
	}
	return parseSeekPenaltyDescriptor(buf[:], n)
}

// storagePropertyQuery 构造 STORAGE_PROPERTY_QUERY 输入缓冲。
//
//	offset 0: STORAGE_PROPERTY_ID PropertyId
//	offset 4: STORAGE_QUERY_TYPE  QueryType
//	offset 8: UCHAR AdditionalParameters[1]
func storagePropertyQuery(propertyID uint32) []byte {
	q := make([]byte, 12)
	binary.LittleEndian.PutUint32(q[0:4], propertyID)
	binary.LittleEndian.PutUint32(q[4:8], propertyStandardQuery)
	return q
}
