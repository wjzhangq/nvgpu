package collector

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/wjzhangq/gpumon/internal/config"
	"github.com/wjzhangq/gpumon/internal/model"
)

// Local 采集运行本程序这台机器的指标。Linux / Windows 通用。
type Local struct {
	name       string
	diskFilter map[string]bool
	diskMode   string
	nvsmi      string

	host model.HostInfo
	topo []socketInfo
}

// NewLocal 构造本机采集器。静态信息（主机名、CPU 拓扑、nvidia-smi 路径）
// 在这里一次性探测完成，后续每个周期只取动态值。
func NewLocal(n config.Node) *Local {
	l := &Local{
		name:       n.Name,
		diskFilter: toSet(n.Disks),
		diskMode:   n.DiskMode,
		nvsmi:      n.NvidiaSmi,
	}
	if l.nvsmi == "" {
		l.nvsmi = defaultNvidiaSmiPath()
	}
	l.host = localHostInfo()
	l.topo = localTopology(l.host.Model)

	// 预热：gopsutil 的 Percent(0, ...) 是"距上次调用"的增量，
	// 先打一次底，避免首个样本变成"自开机以来的平均值"。
	_, _ = cpu.Percent(0, true)
	return l
}

// Name 实现 Collector。
func (l *Local) Name() string { return l.name }

// Close 实现 Collector。本机采集器无需释放资源。
func (l *Local) Close() error { return nil }

// Collect 实现 Collector。
func (l *Local) Collect(ctx context.Context) (model.Snapshot, error) {
	start := time.Now()

	snap := model.Snapshot{
		Node:      l.name,
		Timestamp: start,
		Online:    true,
		Host:      l.host,
	}

	snap.CPUs = l.collectCPUs(ctx)
	snap.Memory = l.collectMemory(ctx)
	if l.diskMode == config.DiskModeBlock {
		snap.Disks = l.collectBlockDevices(ctx)
	} else {
		snap.Disks = l.collectDisks(ctx)
	}
	snap.GPUs = l.collectGPUs(ctx)

	if model.DetectUnifiedMemory(l.host.Arch, snap.Memory.TotalBytes, snap.GPUs) {
		snap.Memory.Unified = true
		for i := range snap.GPUs {
			snap.GPUs[i].Unified = true
		}
	}

	snap.Timestamp = time.Now()
	snap.CollectMS = time.Since(start).Milliseconds()
	return snap, nil
}

func (l *Local) collectCPUs(ctx context.Context) []model.CPU {
	per, err := cpu.PercentWithContext(ctx, 0, true)
	if err != nil {
		per = nil
	}

	out := make([]model.CPU, 0, len(l.topo))
	for i, s := range l.topo {
		c := model.CPU{
			Index:         i,
			Model:         s.model,
			PhysicalCores: s.physCores,
			LogicalCores:  len(s.logical),
		}
		var sum float64
		var n int
		for _, idx := range s.logical {
			if idx >= 0 && idx < len(per) {
				sum += per[idx]
				n++
			}
		}
		if n > 0 {
			c.UsagePercent = round2(clampPercent(sum / float64(n)))
		}
		out = append(out, c)
	}
	return out
}

func (l *Local) collectMemory(ctx context.Context) model.Memory {
	vm, err := mem.VirtualMemoryWithContext(ctx)
	if err != nil || vm == nil {
		return model.Memory{}
	}
	return model.Memory{
		TotalBytes:     vm.Total,
		UsedBytes:      vm.Used,
		AvailableBytes: vm.Available,
		UsagePercent:   round2(clampPercent(vm.UsedPercent)),
	}
}

func (l *Local) collectDisks(ctx context.Context) []model.Disk {
	parts, err := disk.PartitionsWithContext(ctx, false)
	if err != nil {
		return nil
	}

	seen := make(map[string]bool, len(parts))
	var out []model.Disk

	for _, p := range parts {
		mount := p.Mountpoint
		if mount == "" || seen[mount] {
			continue
		}
		whitelisted := l.diskFilter != nil && l.diskFilter[mount]
		if l.diskFilter != nil && !whitelisted {
			continue
		}
		if !whitelisted && isPseudoFS(p.Fstype) {
			continue
		}

		u, err := disk.UsageWithContext(ctx, mount)
		if err != nil || u == nil || u.Total == 0 {
			continue
		}
		if !whitelisted && u.Total < minDiskBytes {
			continue
		}

		seen[mount] = true
		out = append(out, model.Disk{
			Mount:        mount,
			Device:       p.Device,
			FSType:       p.Fstype,
			TotalBytes:   u.Total,
			UsedBytes:    u.Used,
			UsagePercent: round2(clampPercent(u.UsedPercent)),
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out
}

func (l *Local) collectGPUs(ctx context.Context) []model.GPU {
	if l.nvsmi == "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, l.nvsmi, nvidiaArgs()...)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNvidiaCSV(string(out))
}

func (l *Local) collectBlockDevices(ctx context.Context) []model.Disk {
	if runtime.GOOS == "windows" {
		return l.collectBlockDevicesWindows(ctx)
	}
	return l.collectBlockDevicesLinux(ctx)
}

// collectBlockDevicesLinux 从 /sys/block 读取物理磁盘列表（Linux 本机）。
func (l *Local) collectBlockDevicesLinux(ctx context.Context) []model.Disk {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}

	var out []model.Disk
	seen := make(map[string]bool)

	for _, e := range entries {
		name := e.Name()
		if seen[name] {
			continue
		}

		whitelisted := l.diskFilter != nil && (l.diskFilter[name] || l.diskFilter["/dev/"+name])
		if l.diskFilter != nil && !whitelisted {
			continue
		}
		if !whitelisted && isVirtualBlockDevice(name) {
			continue
		}

		sysPath := filepath.Join("/sys/block", name)
		sizeBytes, err := os.ReadFile(filepath.Join(sysPath, "size"))
		if err != nil {
			continue
		}
		sectors := parseUint(strings.TrimSpace(string(sizeBytes)))
		if sectors == 0 {
			continue
		}
		size := sectors * 512

		if !whitelisted && size < minBlockDeviceBytes {
			continue
		}

		var devModel string
		if modelBytes, err := os.ReadFile(filepath.Join(sysPath, "device", "model")); err == nil {
			devModel = strings.TrimSpace(string(modelBytes))
		}

		var rotational *bool
		if rotaBytes, err := os.ReadFile(filepath.Join(sysPath, "queue", "rotational")); err == nil {
			switch strings.TrimSpace(string(rotaBytes)) {
			case "1":
				v := true
				rotational = &v
			case "0":
				v := false
				rotational = &v
			}
		}

		seen[name] = true
		out = append(out, model.Disk{
			Device:     name,
			TotalBytes: size,
			Type:       "disk",
			Model:      devModel,
			Rotational: rotational,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// collectBlockDevicesWindows 使用 wmic 或 PowerShell 采集物理磁盘（Windows 本机）。
func (l *Local) collectBlockDevicesWindows(ctx context.Context) []model.Disk {
	// 优先使用 wmic diskdrive（快，可靠）
	cmd := exec.CommandContext(ctx, "wmic", "diskdrive", "get", "DeviceID,Size,Model,MediaType", "/format:csv")
	out, err := cmd.Output()
	if err == nil {
		return l.parseWmicDiskDrive(string(out))
	}

	// 回退 PowerShell Get-PhysicalDisk（Win8+ 可用，较慢）
	ps := `Get-PhysicalDisk | Select-Object DeviceId,Size,Model,MediaType | ConvertTo-Csv -NoTypeInformation`
	cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", ps)
	out, err = cmd.Output()
	if err != nil {
		return nil
	}
	return l.parsePowerShellDisk(string(out))
}

func (l *Local) parseWmicDiskDrive(csv string) []model.Disk {
	lines := strings.Split(csv, "\n")
	if len(lines) < 2 {
		return nil
	}

	var out []model.Disk
	seen := make(map[string]bool)

	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 5 {
			continue
		}

		deviceID := strings.TrimSpace(f[1])
		if deviceID == "" || seen[deviceID] {
			continue
		}

		size := parseUint(f[3])
		if size == 0 {
			continue
		}

		whitelisted := l.diskFilter != nil && l.diskFilter[deviceID]
		if l.diskFilter != nil && !whitelisted {
			continue
		}
		if !whitelisted && size < minBlockDeviceBytes {
			continue
		}

		devModel := strings.TrimSpace(f[2])
		mediaType := strings.TrimSpace(f[4])

		var rotational *bool
		if strings.Contains(strings.ToLower(mediaType), "ssd") {
			v := false
			rotational = &v
		} else if strings.Contains(strings.ToLower(mediaType), "hdd") {
			v := true
			rotational = &v
		}

		seen[deviceID] = true
		out = append(out, model.Disk{
			Device:     deviceID,
			TotalBytes: size,
			Type:       "disk",
			Model:      devModel,
			Rotational: rotational,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

func (l *Local) parsePowerShellDisk(csv string) []model.Disk {
	lines := strings.Split(csv, "\n")
	if len(lines) < 2 {
		return nil
	}

	var out []model.Disk
	seen := make(map[string]bool)

	for i, line := range lines {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}

		deviceID := strings.Trim(strings.TrimSpace(f[0]), `"`)
		if deviceID == "" || seen[deviceID] {
			continue
		}

		size := parseUint(strings.Trim(strings.TrimSpace(f[1]), `"`))
		if size == 0 {
			continue
		}

		whitelisted := l.diskFilter != nil && l.diskFilter[deviceID]
		if l.diskFilter != nil && !whitelisted {
			continue
		}
		if !whitelisted && size < minBlockDeviceBytes {
			continue
		}

		devModel := strings.Trim(strings.TrimSpace(f[2]), `"`)
		mediaType := strings.Trim(strings.TrimSpace(f[3]), `"`)

		var rotational *bool
		if strings.Contains(strings.ToLower(mediaType), "ssd") {
			v := false
			rotational = &v
		} else if strings.Contains(strings.ToLower(mediaType), "hdd") {
			v := true
			rotational = &v
		}

		seen[deviceID] = true
		out = append(out, model.Disk{
			Device:     deviceID,
			TotalBytes: size,
			Type:       "disk",
			Model:      devModel,
			Rotational: rotational,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
	return out
}

// ---------------------------------------------------------------------------
// 静态信息探测
// ---------------------------------------------------------------------------

func localHostInfo() model.HostInfo {
	h := model.HostInfo{
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}
	h.Hostname, _ = os.Hostname()

	if hi, err := host.Info(); err == nil && hi != nil {
		h.Hostname = firstNonEmpty(hi.Hostname, h.Hostname)
		h.OS = firstNonEmpty(hi.OS, h.OS)
		h.Platform = strings.TrimSpace(hi.Platform + " " + hi.PlatformVersion)
		h.Kernel = hi.KernelVersion
		h.Arch = firstNonEmpty(hi.KernelArch, h.Arch)
	}
	h.Model = localMachineModel()
	return h
}

// localMachineModel 读取整机型号。Linux 上优先 DMI，其次设备树（GB10 这类
// ARM 平台没有 DMI，型号只在 /proc/device-tree/model 里）。其他平台返回空。
func localMachineModel() string {
	candidates := []string{
		"/sys/class/dmi/id/product_name",
		"/sys/firmware/devicetree/base/model",
		"/proc/device-tree/model",
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", ""))
		if s != "" && !strings.EqualFold(s, "To be filled by O.E.M.") {
			return s
		}
	}
	return ""
}

// localTopology 把逻辑核归并到物理 CPU（socket）。
//
// Linux 上 cpu.Info() 每个逻辑核返回一条记录（带 physical id / core id）；
// Windows 上每个物理封装返回一条记录。两种形态都要处理。
func localTopology(machineModel string) []socketInfo {
	numLogical, err := cpu.Counts(true)
	if err != nil || numLogical <= 0 {
		numLogical = runtime.NumCPU()
	}

	infos, err := cpu.Info()
	if err != nil || len(infos) == 0 {
		return []socketInfo{{
			model:     fallbackCPUModel("", machineModel),
			physCores: numLogical,
			logical:   seq(0, numLogical),
		}}
	}

	if len(infos) == numLogical {
		if s := groupByPhysicalID(infos, machineModel); len(s) > 0 {
			return s
		}
	}
	return groupByPackage(infos, numLogical, machineModel)
}

// groupByPhysicalID 处理 Linux 形态：一条记录 = 一个逻辑核。
func groupByPhysicalID(infos []cpu.InfoStat, machineModel string) []socketInfo {
	type acc struct {
		model   string
		cores   map[string]bool
		logical []int
	}
	order := make([]string, 0, 2)
	m := make(map[string]*acc, 2)

	for i, in := range infos {
		key := strings.TrimSpace(in.PhysicalID)
		if key == "" {
			key = "0"
		}
		a, ok := m[key]
		if !ok {
			a = &acc{cores: make(map[string]bool)}
			m[key] = a
			order = append(order, key)
		}
		coreKey := strings.TrimSpace(in.CoreID)
		if coreKey == "" {
			coreKey = strconv.Itoa(i)
		}
		a.cores[coreKey] = true

		// 逻辑核编号以 InfoStat.CPU 为准，它对应 /proc/stat 里的 cpuN。
		idx := int(in.CPU)
		if idx < 0 {
			idx = i
		}
		a.logical = append(a.logical, idx)

		if a.model == "" {
			a.model = strings.TrimSpace(in.ModelName)
		}
	}

	sort.Strings(order)
	out := make([]socketInfo, 0, len(order))
	for _, k := range order {
		a := m[k]
		sort.Ints(a.logical)
		out = append(out, socketInfo{
			model:     fallbackCPUModel(a.model, machineModel),
			physCores: len(a.cores),
			logical:   a.logical,
		})
	}
	return out
}

// groupByPackage 处理 Windows / macOS 形态：一条记录 = 一个物理封装。
func groupByPackage(infos []cpu.InfoStat, numLogical int, machineModel string) []socketInfo {
	n := len(infos)
	per := numLogical / n
	if per < 1 {
		per = 1
	}

	out := make([]socketInfo, 0, n)
	next := 0
	for i, in := range infos {
		count := per
		if i == n-1 {
			// 最后一个封装吃掉余数。
			count = numLogical - next
			if count < 1 {
				count = 1
			}
		}
		cores := int(in.Cores)
		if cores <= 0 {
			cores = count
		}
		out = append(out, socketInfo{
			model:     fallbackCPUModel(in.ModelName, machineModel),
			physCores: cores,
			logical:   seq(next, count),
		})
		next += count
	}
	return out
}

// fallbackCPUModel 兜底 CPU 型号。
// aarch64 的 /proc/cpuinfo 没有 "model name" 字段，gopsutil 会返回空串，
// 这时退回到整机型号（例如 "NVIDIA DGX Spark"）。
func fallbackCPUModel(modelName, machineModel string) string {
	if s := strings.TrimSpace(modelName); s != "" {
		return s
	}
	if s := strings.TrimSpace(machineModel); s != "" {
		return s + " CPU"
	}
	return "unknown (" + runtime.GOARCH + ")"
}

func seq(start, count int) []int {
	if count < 0 {
		count = 0
	}
	out := make([]int, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, start+i)
	}
	return out
}
