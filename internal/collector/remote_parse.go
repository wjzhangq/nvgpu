package collector

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/wjzhangq/gpumon/internal/model"
)

// scriptHeader 统一环境：锁定 C locale（否则 lscpu/df 的字段名会随语言变化），
// 并补齐 PATH——非交互式 SSH 会话拿到的 PATH 通常很窄，nvidia-smi 常常不在里面。
const scriptHeader = `export LC_ALL=C
export PATH="$PATH:/usr/bin:/usr/sbin:/usr/local/bin:/usr/local/sbin:/usr/local/nvidia/bin:/opt/nvidia/bin"
`

// staticScript 只在建立连接时执行一次。
const staticScript = scriptHeader + `echo "#=HOST"
echo "hostname=$(hostname 2>/dev/null)"
echo "os=$(uname -s 2>/dev/null)"
echo "kernel=$(uname -r 2>/dev/null)"
echo "arch=$(uname -m 2>/dev/null)"
echo "platform=$(. /etc/os-release 2>/dev/null; echo "$PRETTY_NAME")"
echo "model=$(cat /sys/class/dmi/id/product_name 2>/dev/null || tr -d '\0' < /proc/device-tree/model 2>/dev/null)"
echo "#=CPUINFO"
cat /proc/cpuinfo 2>/dev/null
echo "#=LSCPU"
lscpu 2>/dev/null || true
echo "#=END"
`

// buildDynamicScript 生成每个采集周期执行的脚本。
// diskMode 为 "block" 时采集物理磁盘，"mount" 时采集挂载点（默认）。
func buildDynamicScript(nvidiaSmi string, diskMode string) string {
	diskCmd := ""
	if diskMode == "block" {
		// 物理磁盘模式：尝试 lsblk，失败则回退到 /sys/block
		diskCmd = `lsblk -b -d -n -o NAME,SIZE,TYPE,MODEL,ROTA 2>/dev/null || {
    for dev in /sys/block/*; do
        name=$(basename "$dev")
        case "$name" in loop*|dm-*|md*|zram*|ram*) continue ;; esac
        [ -f "$dev/size" ] || continue
        sectors=$(cat "$dev/size" 2>/dev/null || echo 0)
        [ "$sectors" = "0" ] && continue
        bytes=$((sectors * 512))
        model=$(cat "$dev/device/model" 2>/dev/null | tr -d ' ' || echo "-")
        rota=$(cat "$dev/queue/rotational" 2>/dev/null || echo "-1")
        echo "$name $bytes disk $model $rota"
    done
}`
	} else {
		// 挂载点模式：保持原有 df 命令
		diskCmd = `df -P -T -B1 2>/dev/null || df -P -k 2>/dev/null || true`
	}

	return scriptHeader + `echo "#=STAT"
grep '^cpu' /proc/stat 2>/dev/null
echo "#=MEM"
grep -E '^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SReclaimable|Shmem):' /proc/meminfo 2>/dev/null
echo "#=DISK"
` + diskCmd + `
echo "#=GPU"
` + shellQuote(nvidiaSmi) + ` --query-gpu=` + nvidiaQuery + ` --format=csv,noheader,nounits 2>/dev/null || true
echo "#=END"
`
}

// shellQuote 用单引号包裹字符串，安全地嵌入 sh 脚本。
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitSections 按 "#=NAME" 标记切分脚本输出。
func splitSections(out string) map[string][]string {
	res := make(map[string][]string, 6)
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "#=") {
			cur = strings.TrimSpace(strings.TrimPrefix(line, "#="))
			if _, ok := res[cur]; !ok {
				res[cur] = []string{}
			}
			continue
		}
		if cur != "" {
			res[cur] = append(res[cur], line)
		}
	}
	return res
}

// parseKeyValues 解析 "key=value" 形式的行。
func parseKeyValues(lines []string) map[string]string {
	m := make(map[string]string, len(lines))
	for _, l := range lines {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// ---------------------------------------------------------------------------
// /proc/stat
// ---------------------------------------------------------------------------

// parseProcStat 解析 cpu / cpu0 / cpu1 ... 行的累计时间。
//
// 字段顺序：user nice system idle iowait irq softirq steal guest guest_nice
// idle 部分取 idle + iowait。
func parseProcStat(lines []string) map[string]cpuTimes {
	res := make(map[string]cpuTimes, len(lines))
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 5 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		var t cpuTimes
		for i, raw := range f[1:] {
			v, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				continue
			}
			t.total += v
			if i == 3 || i == 4 { // idle, iowait
				t.idle += v
			}
		}
		if t.total > 0 {
			t.valid = true
			res[f[0]] = t
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// /proc/meminfo
// ---------------------------------------------------------------------------

// parseMeminfo 解析内存。值单位为 kB。
func parseMeminfo(lines []string) model.Memory {
	kv := make(map[string]uint64, len(lines))
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) == 0 {
			continue
		}
		kv[strings.TrimSpace(k)] = parseUint(f[0]) * 1024
	}

	total := kv["MemTotal"]
	if total == 0 {
		return model.Memory{}
	}

	avail, ok := kv["MemAvailable"]
	if !ok {
		// 老内核没有 MemAvailable，退回经典估算。
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"] + kv["SReclaimable"] - kv["Shmem"]
		if avail > total {
			avail = total
		}
	}
	if avail > total {
		avail = total
	}
	used := total - avail

	return model.Memory{
		TotalBytes:     total,
		UsedBytes:      used,
		AvailableBytes: avail,
		UsagePercent:   round2(clampPercent(model.Percent(used, total))),
	}
}

// ---------------------------------------------------------------------------
// df
// ---------------------------------------------------------------------------

// parseDF 解析 df 输出，同时兼容两种布局：
//   - df -P -T -B1  →  Filesystem Type 1B-blocks Used Available Capacity Mounted-on
//   - df -P -k      →  Filesystem 1024-blocks Used Available Capacity Mounted-on
func parseDF(lines []string, filter map[string]bool) []model.Disk {
	var (
		typed      bool
		mult       = uint64(1)
		haveHeader bool
		out        []model.Disk
		seen       = make(map[string]bool)
	)

	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}

		if !haveHeader {
			if strings.EqualFold(f[0], "Filesystem") {
				haveHeader = true
				typed = len(f) > 1 && strings.EqualFold(f[1], "Type")
				lower := strings.ToLower(l)
				switch {
				case strings.Contains(lower, "1024-blocks"), strings.Contains(lower, "1k-blocks"):
					mult = 1024
				default:
					mult = 1
				}
				continue
			}
			// 没有表头就按最常见的 -T -B1 布局猜。
			haveHeader = true
			typed = true
		}

		var (
			device, fstype, mount string
			totalIdx, usedIdx     int
			mountIdx              int
		)
		if typed {
			if len(f) < 7 {
				continue
			}
			device, fstype = f[0], f[1]
			totalIdx, usedIdx, mountIdx = 2, 3, 6
		} else {
			if len(f) < 6 {
				continue
			}
			device = f[0]
			totalIdx, usedIdx, mountIdx = 1, 2, 5
		}
		mount = strings.Join(f[mountIdx:], " ")
		if mount == "" || seen[mount] {
			continue
		}

		whitelisted := filter != nil && filter[mount]
		if filter != nil && !whitelisted {
			continue
		}
		if !whitelisted && isPseudoFS(fstype) {
			continue
		}

		total := parseUint(f[totalIdx]) * mult
		used := parseUint(f[usedIdx]) * mult
		if total == 0 {
			continue
		}
		if !whitelisted && total < minDiskBytes {
			continue
		}

		seen[mount] = true
		out = append(out, model.Disk{
			Mount:        mount,
			Device:       device,
			FSType:       fstype,
			TotalBytes:   total,
			UsedBytes:    used,
			UsagePercent: round2(clampPercent(model.Percent(used, total))),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

// ---------------------------------------------------------------------------
// 块设备（物理磁盘）
// ---------------------------------------------------------------------------

// parseBlockDevices 解析 block 模式下 DISK 段的输出，兼容两种来源：
//   - lsblk -b -d -n -o NAME,SIZE,TYPE,MODEL,ROTA
//   - /sys/block 回退脚本，格式相同但空字段用 "-" 占位
//
// MODEL 列可能含空格（"Samsung SSD 980 PRO 2TB"），所以从右边定位 ROTA：
// 末字段是 0/1/-1 时才当作 ROTA，其余情况整段算 MODEL。
func parseBlockDevices(lines []string, filter map[string]bool) []model.Disk {
	var out []model.Disk
	seen := make(map[string]bool)

	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}

		name := strings.TrimPrefix(strings.TrimSpace(f[0]), "/dev/")
		if name == "" || seen[name] {
			continue
		}

		whitelisted := filter != nil && (filter[name] || filter["/dev/"+name])
		if filter != nil && !whitelisted {
			continue
		}
		if !whitelisted && isVirtualBlockDevice(name) {
			continue
		}

		size := parseUint(f[1])
		if size == 0 {
			continue
		}

		devType := ""
		if len(f) > 2 {
			devType = f[2]
		}
		// 光驱等非磁盘设备不上报（白名单可以强行拉回来）。
		if !whitelisted && strings.EqualFold(devType, "rom") {
			continue
		}

		var devModel, rota string
		if rest := f[3:]; len(rest) > 0 {
			last := rest[len(rest)-1]
			if last == "0" || last == "1" || last == "-1" {
				rota = last
				rest = rest[:len(rest)-1]
			}
			devModel = strings.Join(rest, " ")
			if devModel == "-" {
				devModel = ""
			}
		}

		var rotational *bool
		switch rota {
		case "1":
			v := true
			rotational = &v
		case "0":
			v := false
			rotational = &v
		}

		if !whitelisted && size < minBlockDeviceBytes {
			continue
		}

		seen[name] = true
		out = append(out, model.Disk{
			Device:     name,
			TotalBytes: size,
			Type:       devType,
			Model:      strings.TrimSpace(devModel),
			Rotational: rotational,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// ---------------------------------------------------------------------------
// CPU 拓扑
// ---------------------------------------------------------------------------

type procCPUEntry struct {
	index int
	phys  string
	core  string
	model string
}

// parseProcCPUInfo 解析 /proc/cpuinfo，每个 "processor:" 开一条新记录。
func parseProcCPUInfo(lines []string) []procCPUEntry {
	var (
		entries []procCPUEntry
		cur     procCPUEntry
		started bool
	)

	flush := func() {
		if started {
			entries = append(entries, cur)
		}
	}

	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)

		if k == "processor" {
			flush()
			idx, err := strconv.Atoi(v)
			if err != nil {
				idx = len(entries)
			}
			cur = procCPUEntry{index: idx}
			started = true
			continue
		}
		if !started {
			continue
		}
		switch k {
		case "physical id":
			cur.phys = v
		case "core id":
			cur.core = v
		case "model name":
			cur.model = v
		}
	}
	flush()
	return entries
}

// parseLscpu 把 lscpu 的 "Key: Value" 输出转成小写键的 map。
func parseLscpu(lines []string) map[string]string {
	m := make(map[string]string, len(lines))
	for _, l := range lines {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(k))
		if key == "" {
			continue
		}
		if _, exists := m[key]; !exists {
			m[key] = strings.TrimSpace(v)
		}
	}
	return m
}

// buildRemoteTopology 把逻辑核归并成物理 CPU。
//
// x86 的 /proc/cpuinfo 自带 physical id / core id / model name，直接分组即可；
// aarch64（GB10、Grace 等）这三项全都没有，此时退回 lscpu，
// 再退回整机型号，保证 model 字段不会是空串。
func buildRemoteTopology(entries []procCPUEntry, lscpu map[string]string, h model.HostInfo) []socketInfo {
	machineModel := ""
	if s := strings.TrimSpace(h.Model); s != "" {
		machineModel = s + " CPU"
	}
	fallbackModel := firstNonEmpty(
		lscpu["model name"],
		lscpu["bios model name"],
		machineModel,
		"unknown ("+strings.TrimSpace(h.Arch)+")",
	)
	coresPerSocket := atoiSafe(lscpu["core(s) per socket"])

	if len(entries) == 0 {
		// 连 /proc/cpuinfo 都拿不到，只能靠 lscpu 拼一个。
		total := atoiSafe(lscpu["cpu(s)"])
		if total <= 0 {
			total = 1
		}
		sockets := atoiSafe(lscpu["socket(s)"])
		if sockets <= 0 {
			sockets = 1
		}
		per := total / sockets
		if per < 1 {
			per = 1
		}
		out := make([]socketInfo, 0, sockets)
		next := 0
		for i := 0; i < sockets; i++ {
			count := per
			if i == sockets-1 {
				count = total - next
				if count < 1 {
					count = 1
				}
			}
			cores := coresPerSocket
			if cores <= 0 {
				cores = count
			}
			out = append(out, socketInfo{model: fallbackModel, physCores: cores, logical: seq(next, count)})
			next += count
		}
		return out
	}

	type acc struct {
		model   string
		cores   map[string]bool
		logical []int
	}
	order := make([]string, 0, 2)
	m := make(map[string]*acc, 2)

	for i, e := range entries {
		key := e.phys
		if key == "" {
			key = "0"
		}
		a, ok := m[key]
		if !ok {
			a = &acc{cores: make(map[string]bool)}
			m[key] = a
			order = append(order, key)
		}
		coreKey := e.core
		if coreKey == "" {
			// 没有 core id（arm64）：每个逻辑核算一个物理核。
			coreKey = fmt.Sprintf("i%d", i)
		}
		a.cores[coreKey] = true
		a.logical = append(a.logical, e.index)
		if a.model == "" {
			a.model = e.model
		}
	}

	sort.Slice(order, func(i, j int) bool {
		a, errA := strconv.Atoi(order[i])
		b, errB := strconv.Atoi(order[j])
		if errA == nil && errB == nil {
			return a < b
		}
		return order[i] < order[j]
	})

	out := make([]socketInfo, 0, len(order))
	for _, k := range order {
		a := m[k]
		sort.Ints(a.logical)
		cores := len(a.cores)
		if coresPerSocket > 0 {
			cores = coresPerSocket
		}
		out = append(out, socketInfo{
			model:     firstNonEmpty(a.model, fallbackModel),
			physCores: cores,
			logical:   a.logical,
		})
	}
	return out
}

func atoiSafe(s string) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return v
}
