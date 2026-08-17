package collector

import "strings"

// normalizeMountKey 归一化挂载点键，用于白名单匹配与上报。
//
// 跨平台实现：Windows 盘符统一成 "C:\"（大写 + 反斜杠），Unix 路径原样返回。
// 放在跨平台文件里而不是 _windows.go，是为了让归一化逻辑在任何平台都能被
// 测试覆盖 —— 这段逻辑没有系统调用，没必要按平台分裂。
//
// 之所以需要它：Windows 上 gopsutil 报的挂载点是 "C:"（无反斜杠），而用户
// 在配置里习惯写 "C:\" 或 "c:"，精确匹配必然落空。
func normalizeMountKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// 只对 "X:" / "X:\" / "X:/" 这三种盘符形式做归一化。
	// 其余（Unix 绝对路径、UNC 路径等）原样返回。
	if len(s) >= 2 && s[1] == ':' && isDriveLetter(s[0]) {
		switch len(s) {
		case 2:
			return strings.ToUpper(s[:2]) + `\`
		case 3:
			if s[2] == '\\' || s[2] == '/' {
				return strings.ToUpper(s[:2]) + `\`
			}
		}
	}
	return s
}

// normalizeBlockKey 归一化块设备键，用于白名单匹配。
//
// 跨平台实现，同时处理两种命名体系：
//   - Windows: "0" / "PhysicalDrive0" / `\\.\PHYSICALDRIVE0` → "PhysicalDrive0"
//   - Linux:   "sda" / "/dev/sda" → "sda"
//
// 纯数字一律按 Windows 物理磁盘号解释 —— Linux 上不存在纯数字的块设备名，
// 所以这个判据没有歧义。
func normalizeBlockKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// Linux 风格：去掉 /dev/ 前缀
	if strings.HasPrefix(s, "/dev/") {
		return strings.TrimPrefix(s, "/dev/")
	}

	// Windows 风格：剥掉 \\.\ 与 PhysicalDrive 前缀，只留磁盘号
	upper := strings.ToUpper(s)
	upper = strings.TrimPrefix(upper, `\\.\`)
	if rest, ok := trimPrefixFold(upper, "PHYSICALDRIVE"); ok {
		if isAllDigits(rest) {
			return "PhysicalDrive" + rest
		}
		return s
	}

	// 裸数字：按 Windows 物理磁盘号解释
	if isAllDigits(s) {
		return "PhysicalDrive" + s
	}

	return s
}

func isDriveLetter(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// trimPrefixFold 剥掉大小写不敏感的前缀，返回剩余部分与是否命中。
// s 应当已经是大写形式。
func trimPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}
