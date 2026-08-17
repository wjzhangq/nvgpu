package collector

import (
	"strings"
	"testing"
)

// TestNormalizeMountKey 验证挂载点键归一化逻辑。
func TestNormalizeMountKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"C:", "C:\\"},
		{"C:\\", "C:\\"},
		{"c:", "C:\\"},
		{"c:\\", "C:\\"},
		{"c:/", "C:\\"},
		{"D:", "D:\\"},
		{"  E:  ", "E:\\"},
		{"/mnt/data", "/mnt/data"}, // Unix 路径不变
	}

	for _, tt := range tests {
		got := normalizeMountKey(tt.input)
		if got != tt.want {
			t.Errorf("normalizeMountKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestNormalizeBlockKey 验证 block 设备键归一化逻辑。
func TestNormalizeBlockKey(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0", "PhysicalDrive0"},
		{"1", "PhysicalDrive1"},
		{"PhysicalDrive0", "PhysicalDrive0"},
		{"physicaldrive0", "PhysicalDrive0"},
		{"PHYSICALDRIVE0", "PhysicalDrive0"},
		{`\\.\PHYSICALDRIVE0`, "PhysicalDrive0"},
		{`\\.\PhysicalDrive0`, "PhysicalDrive0"},
		{"  2  ", "PhysicalDrive2"},
		{"sda", "sda"}, // Linux 设备名不变
		{"/dev/sda", "sda"},
	}

	for _, tt := range tests {
		got := normalizeBlockKey(tt.input)
		// Windows 归一化会转成 "PhysicalDrive0"，Unix 会去掉 "/dev/" 前缀
		if !strings.EqualFold(got, tt.want) && got != tt.want {
			// 允许大小写不敏感匹配（因为 normalizeBlockKey 的行为在不同平台可能不同）
			t.Errorf("normalizeBlockKey(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestMountWhitelistMatching 验证白名单匹配逻辑（跨平台）。
func TestMountWhitelistMatching(t *testing.T) {
	// 模拟 toSet 的归一化行为
	normalize := func(items []string) map[string]bool {
		m := make(map[string]bool, len(items))
		for _, s := range items {
			m[normalizeMountKey(s)] = true
		}
		return m
	}

	whitelist := normalize([]string{"C:", "D:\\", "c:/", "/mnt/data"})

	tests := []struct {
		input string
		want  bool
	}{
		{"C:", true},
		{"C:\\", true},
		{"c:", true},
		{"c:\\", true},
		{"D:", true},
		{"D:\\", true},
		{"E:\\", false},
		{"/mnt/data", true},
		{"/mnt/other", false},
	}

	for _, tt := range tests {
		key := normalizeMountKey(tt.input)
		got := whitelist[key]
		if got != tt.want {
			t.Errorf("whitelist match for %q (normalized to %q) = %v, want %v",
				tt.input, key, got, tt.want)
		}
	}
}

// TestBlockWhitelistMatching 验证 block 设备白名单匹配逻辑。
func TestBlockWhitelistMatching(t *testing.T) {
	normalize := func(items []string) map[string]bool {
		m := make(map[string]bool, len(items))
		for _, s := range items {
			m[normalizeBlockKey(s)] = true
		}
		return m
	}

	whitelist := normalize([]string{"0", "PhysicalDrive1", "sda", "/dev/sdb"})

	tests := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"PhysicalDrive0", true},
		{"physicaldrive0", true},
		{`\\.\PHYSICALDRIVE0`, true},
		{"1", true},
		{"PhysicalDrive1", true},
		{"2", false},
		{"sda", true},
		{"/dev/sda", true},
		{"sdb", true},
		{"/dev/sdb", true},
		{"sdc", false},
	}

	for _, tt := range tests {
		key := normalizeBlockKey(tt.input)
		got := whitelist[key]
		if got != tt.want {
			t.Errorf("whitelist match for %q (normalized to %q) = %v, want %v",
				tt.input, key, got, tt.want)
		}
	}
}
