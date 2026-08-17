package collector

import (
	"encoding/binary"
	"testing"
)

// TestParseDiskGeometryEx 验证 DISK_GEOMETRY_EX 缓冲解析。
func TestParseDiskGeometryEx(t *testing.T) {
	// 构造一个最小合法缓冲：前 24 字节是 DISK_GEOMETRY，offset 24 是 DiskSize
	buf := make([]byte, 256)
	diskSize := uint64(2000398934016) // 约 2TB
	binary.LittleEndian.PutUint64(buf[24:32], diskSize)

	got, err := parseDiskGeometryEx(buf, 256)
	if err != nil {
		t.Fatalf("parseDiskGeometryEx 失败: %v", err)
	}
	if got != diskSize {
		t.Errorf("DiskSize = %d, want %d", got, diskSize)
	}

	// 缓冲过短
	_, err = parseDiskGeometryEx(buf[:20], 20)
	if err == nil {
		t.Error("缓冲过短时应返回错误")
	}
}

// TestParseStorageDeviceDescriptor 验证 STORAGE_DEVICE_DESCRIPTOR 解析。
func TestParseStorageDeviceDescriptor(t *testing.T) {
	tests := []struct {
		name             string
		removableByte    byte
		vendorOff        uint32
		productOff       uint32
		vendor           string
		product          string
		wantModel        string
		wantRemovable    bool
	}{
		{
			name:          "Samsung SSD 980 PRO",
			removableByte: 0,
			vendorOff:     40,
			productOff:    48,
			vendor:        "Samsung",
			product:       "SSD 980 PRO 2TB",
			wantModel:     "Samsung SSD 980 PRO 2TB",
			wantRemovable: false,
		},
		{
			name:          "可移动 USB 盘",
			removableByte: 1,
			vendorOff:     40,
			productOff:    52,
			vendor:        "SanDisk",
			product:       "Ultra USB 3.0",
			wantModel:     "SanDisk Ultra USB 3.0",
			wantRemovable: true,
		},
		{
			name:          "只有 ProductId",
			removableByte: 0,
			vendorOff:     0, // 0 表示字段不可用
			productOff:    40,
			product:       "Generic HDD",
			wantModel:     "Generic HDD",
			wantRemovable: false,
		},
		{
			name:          "两个字段都缺失",
			removableByte: 0,
			vendorOff:     0,
			productOff:    0,
			wantModel:     "Unknown",
			wantRemovable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := make([]byte, 256)
			// RemovableMedia @ offset 10
			buf[10] = tt.removableByte
			// VendorIdOffset @ offset 12
			binary.LittleEndian.PutUint32(buf[12:16], tt.vendorOff)
			// ProductIdOffset @ offset 16
			binary.LittleEndian.PutUint32(buf[16:20], tt.productOff)

			// 写入 NUL 结尾的 ASCII 字符串
			if tt.vendorOff > 0 && tt.vendorOff < 256 {
				copy(buf[tt.vendorOff:], append([]byte(tt.vendor), 0))
			}
			if tt.productOff > 0 && tt.productOff < 256 {
				copy(buf[tt.productOff:], append([]byte(tt.product), 0))
			}

			model, removable, err := parseStorageDeviceDescriptor(buf, 256)
			if err != nil {
				t.Fatalf("parseStorageDeviceDescriptor 失败: %v", err)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}
			if removable != tt.wantRemovable {
				t.Errorf("removable = %v, want %v", removable, tt.wantRemovable)
			}
		})
	}
}

// TestParseSeekPenaltyDescriptor 验证 DEVICE_SEEK_PENALTY_DESCRIPTOR 解析。
func TestParseSeekPenaltyDescriptor(t *testing.T) {
	tests := []struct {
		name            string
		incursSeekPenalty byte
		wantRotational  bool
	}{
		{"机械盘", 1, true},
		{"SSD", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := make([]byte, 64)
			buf[8] = tt.incursSeekPenalty

			rotational, err := parseSeekPenaltyDescriptor(buf, 64)
			if err != nil {
				t.Fatalf("parseSeekPenaltyDescriptor 失败: %v", err)
			}
			if rotational != tt.wantRotational {
				t.Errorf("rotational = %v, want %v", rotational, tt.wantRotational)
			}
		})
	}
}
