// Package model 定义所有节点通用的指标数据结构。
//
// 单位约定：所有容量类字段一律使用 **字节 (bytes)**，百分比字段为 0-100 的
// float64（保留两位小数）。客户端自己换算 GB，避免服务端做有损取整。
package model

import (
	"strings"
	"time"
)

// HostInfo 描述一台机器的静态身份信息。
type HostInfo struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`                 // linux / windows
	Platform string `json:"platform,omitempty"` // Ubuntu 24.04.1 LTS
	Kernel   string `json:"kernel,omitempty"`   // 6.11.0-1004-nvidia
	Arch     string `json:"arch"`               // x86_64 / aarch64
	Model    string `json:"model,omitempty"`    // ThinkStation PX / NVIDIA DGX Spark
}

// CPU 表示一颗物理 CPU（socket）。多路机器会有多个元素。
type CPU struct {
	Index         int     `json:"index"`
	Model         string  `json:"model"`
	PhysicalCores int     `json:"physical_cores"`
	LogicalCores  int     `json:"logical_cores"`
	UsagePercent  float64 `json:"usage_percent"`
}

// Memory 表示系统内存。
type Memory struct {
	TotalBytes     uint64  `json:"total_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsagePercent   float64 `json:"usage_percent"`

	// Unified 为 true 表示该机器是统一内存架构（如 GB10 / Grace-Blackwell），
	// 系统内存与 GPU 显存是同一块物理内存。
	//
	// 注意：本字段**仅作提示**。按既定设计，memory 与 gpus[].vram_* 仍各自
	// 独立上报完整数值，调用方若要做跨机汇总，需要自行决定是否去重。
	Unified bool `json:"unified"`
}

// GPU 表示一块 NVIDIA 加速卡。
type GPU struct {
	Index              int     `json:"index"`
	Model              string  `json:"model"`
	UUID               string  `json:"uuid,omitempty"`
	UtilizationPercent float64 `json:"utilization_percent"`
	VRAMTotalBytes     uint64  `json:"vram_total_bytes"`
	VRAMUsedBytes      uint64  `json:"vram_used_bytes"`
	VRAMUsagePercent   float64 `json:"vram_usage_percent"`

	// Unified 含义同 Memory.Unified。
	Unified bool `json:"unified"`
}

// Disk 表示一个磁盘资源。按节点的 disk_mode 有两种语义：
//
//   - mount 模式（默认）：一个已挂载的文件系统。Mount 非空，
//     TotalBytes / UsedBytes 是文件系统层面的容量与用量。
//   - block 模式：一块物理块设备。Mount 为空，TotalBytes 是磁盘原始容量
//     （即 /sys/block/<dev>/size × 512），Type / Model / Rotational 有值。
//     **UsedBytes 与 UsagePercent 恒为 0**——块设备层面没有"已用"概念，
//     要算用量得聚合其所有分区的挂载点，当前版本不做。
type Disk struct {
	Mount        string  `json:"mount"`
	Device       string  `json:"device,omitempty"`
	FSType       string  `json:"fstype,omitempty"`
	TotalBytes   uint64  `json:"total_bytes"`
	UsedBytes    uint64  `json:"used_bytes"`
	UsagePercent float64 `json:"usage_percent"`

	// 以下字段只在 block 模式下有值。
	Type  string `json:"type,omitempty"`  // lsblk 的 TYPE 列，通常是 disk
	Model string `json:"model,omitempty"` // 磁盘型号，如 Samsung SSD 980 PRO 2TB
	// Rotational: true = 机械盘，false = 固态盘，nil = 未知。
	Rotational *bool `json:"rotational,omitempty"`
}

// Snapshot 是一个节点在某一时刻的完整指标快照，也是历史环形缓冲的元素类型。
type Snapshot struct {
	Node      string    `json:"node"`
	Timestamp time.Time `json:"timestamp"`
	Online    bool      `json:"online"`
	Error     string    `json:"error,omitempty"`
	CollectMS int64     `json:"collect_ms"`

	Host   HostInfo `json:"host"`
	CPUs   []CPU    `json:"cpus"`
	Memory Memory   `json:"memory"`
	GPUs   []GPU    `json:"gpus"`
	Disks  []Disk   `json:"disks"`
}

// Percent 计算 used/total 的百分比，total 为 0 时返回 0。
func Percent(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// DetectUnifiedMemory 判断一台机器是否为统一内存架构。
//
// 判据（与厂商型号解耦，对 GB10 / GH200 / Thor 等一视同仁）：
//  1. CPU 架构为 arm/aarch64；
//  2. 存在 GPU；
//  3. GPU 显存总量与系统内存总量差距在 25% 以内。
//
// 独显机器上 VRAM 与 RAM 的比例几乎不可能落进这个区间，所以误判风险很低。
func DetectUnifiedMemory(arch string, memTotal uint64, gpus []GPU) bool {
	if memTotal == 0 || len(gpus) == 0 {
		return false
	}
	a := strings.ToLower(arch)
	if !strings.Contains(a, "arm") && !strings.Contains(a, "aarch") {
		return false
	}
	var vram uint64
	for _, g := range gpus {
		vram += g.VRAMTotalBytes
	}
	if vram == 0 {
		return false
	}
	diff := float64(vram) - float64(memTotal)
	if diff < 0 {
		diff = -diff
	}
	return diff/float64(memTotal) < 0.25
}
