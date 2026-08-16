// Package collector 实现本机与远程 SSH 两种指标采集器。
package collector

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/wjzhangq/gpumon/internal/model"
)

// Collector 是采集器的统一接口。
//
// 约定：Collect 即使返回 error，也应尽量返回一个填好 Node/Timestamp 的
// Snapshot，便于上层直接落库为一条"离线"记录。
type Collector interface {
	Name() string
	Collect(ctx context.Context) (model.Snapshot, error)
	Close() error
}

// nvidiaQuery 是本机与远程共用的 nvidia-smi 查询字段。
const nvidiaQuery = "index,name,uuid,utilization.gpu,memory.total,memory.used,temperature.gpu,power.draw"

// nvidiaArgs 返回完整的 nvidia-smi 参数列表。
func nvidiaArgs() []string {
	return []string{"--query-gpu=" + nvidiaQuery, "--format=csv,noheader,nounits"}
}

const mib = uint64(1024 * 1024)

// parseNvidiaCSV 解析 nvidia-smi 的 CSV 输出。
// 字段顺序必须与 nvidiaQuery 一致：index,name,uuid,util,mem.total,mem.used,temp,power。
// memory 单位 MiB，temperature 单位 ℃，power 单位 W。
// 无法解析的行（例如驱动报错文本、"[N/A]"）会被跳过。
func parseNvidiaCSV(out string) []model.GPU {
	var gpus []model.GPU
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 8 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}

		idx, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		total := parseUint(f[4]) * mib
		used := parseUint(f[5]) * mib
		temp := uint32(parseUint(f[6]))   // temperature.gpu (℃)
		power := round2(parseFloat(f[7])) // power.draw (W)

		gpus = append(gpus, model.GPU{
			Index:              idx,
			Model:              f[1],
			UUID:               f[2],
			UtilizationPercent: round2(parseFloat(f[3])),
			VRAMTotalBytes:     total,
			VRAMUsedBytes:      used,
			VRAMUsagePercent:   round2(model.Percent(used, total)),
			TemperatureCelsius: temp,
			PowerWatts:         power,
		})
	}
	return gpus
}

// socketInfo 描述一颗物理 CPU 及其包含的逻辑核编号。
type socketInfo struct {
	model     string
	physCores int
	logical   []int
}

// cpuTimes 是 /proc/stat 一行的累计时间，用于计算两次采集之间的增量。
type cpuTimes struct {
	total uint64
	idle  uint64
	valid bool
}

// usageBetween 根据前后两次累计值计算使用率百分比。
func usageBetween(prev, cur cpuTimes) (float64, bool) {
	if !prev.valid || !cur.valid {
		return 0, false
	}
	if cur.total < prev.total || cur.idle < prev.idle {
		// 计数器回绕或机器重启，本轮丢弃。
		return 0, false
	}
	dt := cur.total - prev.total
	if dt == 0 {
		return 0, false
	}
	di := cur.idle - prev.idle
	u := (1 - float64(di)/float64(dt)) * 100
	return clampPercent(u), true
}

// pseudoFS 是需要排除的伪文件系统/容器层。
var pseudoFS = map[string]bool{
	"autofs": true, "binfmt_misc": true, "bpf": true, "cgroup": true,
	"cgroup2": true, "configfs": true, "debugfs": true, "devfs": true,
	"devpts": true, "devtmpfs": true, "efivarfs": true, "fuse.gvfsd-fuse": true,
	"fuse.portal": true, "fusectl": true, "hugetlbfs": true, "iso9660": true,
	"mqueue": true, "nsfs": true, "overlay": true, "proc": true,
	"pstore": true, "ramfs": true, "rpc_pipefs": true, "securityfs": true,
	"squashfs": true, "sysfs": true, "tmpfs": true, "tracefs": true,
	"udev": true,
}

func isPseudoFS(fstype string) bool {
	f := strings.ToLower(strings.TrimSpace(fstype))
	if f == "" {
		return false
	}
	if pseudoFS[f] {
		return true
	}
	// snap 的 loop 挂载、以及各类 fuse 桥接
	return strings.HasPrefix(f, "fuse.")
}

// minDiskBytes：自动发现模式下忽略小于 1 GiB 的挂载点（多为系统分区噪声）。
const minDiskBytes = uint64(1) << 30

// minBlockDeviceBytes：block 模式下忽略小于 10 GiB 的设备（USB / 系统小盘）。
const minBlockDeviceBytes = uint64(10) << 30

// isVirtualBlockDevice 判断设备名是否为虚拟块设备（loop / dm / md / zram 等）。
func isVirtualBlockDevice(name string) bool {
	prefixes := []string{"loop", "dm-", "md", "zram", "ram", "nbd"}
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func parseUint(s string) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	m := make(map[string]bool, len(items))
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it != "" {
			m[it] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
