package collector

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// 本文件是 Windows IOCTL 返回缓冲的纯解析逻辑，故意不加 build tag：
// 这样偏移量和边界检查可以在任意平台上做单元测试，不需要真实 Windows 机器。

// parseDiskGeometryEx 从 IOCTL_DISK_GET_DRIVE_GEOMETRY_EX 的返回缓冲解析磁盘容量。
//
// DISK_GEOMETRY_EX 布局（x64）：
//
//	offset  0: DISK_GEOMETRY Geometry (24 bytes)
//	offset 24: LARGE_INTEGER DiskSize (8 bytes)
//	offset 32: UCHAR Data[1]
func parseDiskGeometryEx(buf []byte, n uint32) (uint64, error) {
	if n < 32 || len(buf) < 32 {
		return 0, fmt.Errorf("DISK_GEOMETRY_EX 缓冲过短: n=%d len=%d", n, len(buf))
	}
	return binary.LittleEndian.Uint64(buf[24:32]), nil
}

// parseStorageDeviceDescriptor 从 IOCTL_STORAGE_QUERY_PROPERTY +
// StorageDeviceProperty 的返回缓冲解析型号与可移动标记。
//
// STORAGE_DEVICE_DESCRIPTOR 布局：
//
//	offset  0: ULONG Version
//	offset  4: ULONG Size
//	offset  8: UCHAR DeviceType
//	offset  9: UCHAR DeviceTypeModifier
//	offset 10: BOOLEAN RemovableMedia
//	offset 11: BOOLEAN CommandQueueing
//	offset 12: ULONG VendorIdOffset
//	offset 16: ULONG ProductIdOffset
//	offset 20: ULONG ProductRevisionOffset
//	offset 24: ULONG SerialNumberOffset
//	offset 28: STORAGE_BUS_TYPE BusType
//
// VendorId / ProductId 是相对缓冲起始的字节偏移，指向 NUL 结尾的 ASCII 串；
// 偏移为 0 表示该字段不可用。
func parseStorageDeviceDescriptor(buf []byte, n uint32) (modelStr string, removable bool, err error) {
	if n < 20 || len(buf) < 20 {
		return "", false, fmt.Errorf("STORAGE_DEVICE_DESCRIPTOR 缓冲过短: n=%d len=%d", n, len(buf))
	}

	limit := int(n)
	if limit > len(buf) {
		limit = len(buf)
	}

	removable = buf[10] != 0
	vendorOff := binary.LittleEndian.Uint32(buf[12:16])
	productOff := binary.LittleEndian.Uint32(buf[16:20])

	var parts []string
	if s := readNullTermString(buf[:limit], int(vendorOff)); s != "" {
		parts = append(parts, s)
	}
	if s := readNullTermString(buf[:limit], int(productOff)); s != "" {
		parts = append(parts, s)
	}

	modelStr = strings.TrimSpace(strings.Join(parts, " "))
	if modelStr == "" {
		modelStr = "Unknown"
	}
	return modelStr, removable, nil
}

// parseSeekPenaltyDescriptor 从 IOCTL_STORAGE_QUERY_PROPERTY +
// StorageDeviceSeekPenaltyProperty 的返回缓冲解析寻道惩罚标记。
//
// DEVICE_SEEK_PENALTY_DESCRIPTOR 布局：
//
//	offset 0: ULONG Version
//	offset 4: ULONG Size
//	offset 8: BOOLEAN IncursSeekPenalty
//
// IncursSeekPenalty=true 表示机械盘（rotational），false 表示 SSD。
func parseSeekPenaltyDescriptor(buf []byte, n uint32) (rotational bool, err error) {
	if n < 9 || len(buf) < 9 {
		return false, fmt.Errorf("DEVICE_SEEK_PENALTY_DESCRIPTOR 缓冲过短: n=%d len=%d", n, len(buf))
	}
	return buf[8] != 0, nil
}

// readNullTermString 从 buf[offset:] 读取 NUL 结尾的 ASCII 字符串。
// offset 为 0 或越界时返回空串（Windows 用 0 表示"字段不可用"）。
func readNullTermString(buf []byte, offset int) string {
	if offset <= 0 || offset >= len(buf) {
		return ""
	}
	end := offset
	for end < len(buf) && buf[end] != 0 {
		end++
	}
	return strings.TrimSpace(string(buf[offset:end]))
}
