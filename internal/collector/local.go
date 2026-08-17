package collector

import (
	"context"
	"os"
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
	"github.com/wjzhangq/gpumon/internal/logx"
	"github.com/wjzhangq/gpumon/internal/model"
)

// defaultPartitionCacheTTL 是挂载点缓存的默认存活时间。
// 挂载点变化频率极低（分钟级以上），缓存可省下每周期 5-20ms 的扫描开销。
const defaultPartitionCacheTTL = 60 * time.Second

// nvsmiReprobeInterval 是 nvidia-smi 路径重探测的最小间隔。
// 探测要遍历 DriverStore 下的多个候选目录，不能每个采集周期都跑一遍。
const nvsmiReprobeInterval = 30 * time.Second

// defaultGPUTimeout 是 GPU 单次采集的默认超时。
// 独立于节点 interval：Windows 上 nvidia-smi 冷启动（驱动加载 + 卡从低功耗态唤醒）
// 常见 1-4s，若沿用 min(collect_timeout, interval) 的 2s 预算会稳定超时。
const defaultGPUTimeout = 10 * time.Second

// Local 采集运行本程序这台机器的指标。Linux / Windows 通用。
type Local struct {
	name       string
	diskFilter map[string]bool
	diskMode   string
	nvsmi      string

	host model.HostInfo
	topo []socketInfo

	// GPU 异步采集器（单飞 + 缓存），解决 Windows 上 nvidia-smi 冷启动耗时导致的超时问题
	gpuSampler *gpuSampler
	// gpuWarned 是上一次打印过的 GPU 告警文本，用于日志去重。
	gpuWarned string
	// nvsmiProbedAt 是上次 nvidia-smi 路径探测的时刻，用于 30s 退避
	// （路径探测要扫 DriverStore 目录，不能每 2s 跑一遍）。
	nvsmiProbedAt time.Time

	// 磁盘分区缓存（仅 mount 模式）：挂载点变化频率极低，缓存可减少 5-20ms 开销
	partitionsCache     []disk.PartitionStat
	partitionsCacheTime time.Time
	partitionsCacheTTL  time.Duration // 默认 60 秒
}

// NewLocal 构造本机采集器。静态信息（主机名、CPU 拓扑、nvidia-smi 路径）
// 在这里一次性探测完成，后续每个周期只取动态值。
func NewLocal(n config.Node) *Local {
	// 未指定 partition_cache_ttl 时用默认 60s；显式设为 0 表示禁用缓存。
	ttl := defaultPartitionCacheTTL
	if n.PartitionCacheTTL != nil {
		ttl = n.PartitionCacheTTL.Duration
	}

	// GPU 超时解耦于采集轮次的 interval，让 Windows 上 nvidia-smi 冷启动有足够时间
	gpuTimeout := defaultGPUTimeout
	if n.GPUTimeout != nil && n.GPUTimeout.Duration > 0 {
		gpuTimeout = n.GPUTimeout.Duration
	}

	l := &Local{
		name:               n.Name,
		diskFilter:         toSet(n.Disks),
		diskMode:           n.DiskMode,
		nvsmi:              n.NvidiaSmi,
		partitionsCacheTTL: ttl,
	}
	if l.nvsmi == "" {
		l.nvsmi = defaultNvidiaSmiPath()
	}

	// 启动时打印 nvidia-smi 路径探测结果（-v 下逐条打印候选扫描过程）
	if l.nvsmi != "" {
		logx.Infof("nvidia-smi 路径: %s", l.nvsmi)
	} else {
		logx.Infof("nvidia-smi 未找到，GPU 采集将跳过")
	}

	l.host = localHostInfo()
	l.topo = localTopology(l.host.Model)

	// 预热：gopsutil 的 Percent(0, ...) 是"距上次调用"的增量，
	// 先打一次底，避免首个样本变成"自开机以来的平均值"。
	_, _ = cpu.Percent(0, true)

	// 初始化 GPU 采集器并预热（首屏就有数据）
	l.gpuSampler = newGPUSampler(l.name, n.Interval.Duration, gpuTimeout, l.collectGPUsSync)
	l.gpuSampler.warmup()

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

	// GPU 采集走异步缓存路径，不阻塞本轮（sampler 内部单飞 + 超时脱离 ctx）
	gpus, gpusSampledAt := l.gpuSampler.get()
	snap.GPUs = gpus
	// 当 GPU 采样时刻明显早于快照时刻时填充该字段，让 API 消费方知晓数据年龄
	if gpus != nil && time.Since(gpusSampledAt) > 5*time.Second {
		snap.GPUsSampledAt = &gpusSampledAt
	}

	if model.DetectUnifiedMemory(l.host.Arch, snap.Memory.TotalBytes, snap.GPUs) {
		snap.Memory.Unified = true
		for i := range snap.GPUs {
			snap.GPUs[i].Unified = true
		}
	}

	snap.Timestamp = time.Now()
	snap.CollectMS = time.Since(start).Milliseconds()

	if logx.Verbose() {
		logx.Debugf("node %q 采集完成: cpu=%dms mem=%dms disk=%dms gpu=cached total=%dms",
			l.name, 0, 0, 0, snap.CollectMS) // 子系统耗时暂不埋点，先有总耗时
	}

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
	now := time.Now()
	var parts []disk.PartitionStat
	var err error

	// 缓存未过期：复用（挂载点变化频率极低，缓存节省 5-20ms）
	if now.Sub(l.partitionsCacheTime) < l.partitionsCacheTTL && len(l.partitionsCache) > 0 {
		parts = l.partitionsCache
	} else {
		// 缓存过期：重新扫描（调用平台钩子：Windows 过滤 DRIVE_FIXED，Unix 走 gopsutil）
		parts, err = listPartitions(ctx, l.diskFilter)
		if err != nil {
			return nil
		}
		l.partitionsCache = parts
		l.partitionsCacheTime = now
	}

	seen := make(map[string]bool, len(parts))
	var out []model.Disk

	for _, p := range parts {
		// 上报值也走归一化：Windows 上 gopsutil 报 "C:"，归一化成 "C:\" 后
		// 前端显示和用户在配置里写的形式一致，历史数据的 key 也稳定。
		mount := normalizeMountKey(p.Mountpoint)
		if mount == "" || seen[mount] {
			continue
		}
		// 白名单匹配走归一化键：Windows 上配置写 "C:" / "c:\" / "C:/" 都能命中 "C:\"
		whitelisted := l.diskFilter != nil && l.diskFilter[mount]
		if l.diskFilter != nil && !whitelisted {
			if logx.Verbose() {
				logx.Debugf("挂载点 %s 不在白名单，跳过", mount)
			}
			continue
		}
		if !whitelisted && isPseudoFS(p.Fstype) {
			continue
		}

		// 平台钩子：Windows 直接调 GetDiskFreeSpaceEx，Unix 走 gopsutil
		u, err := diskUsage(ctx, mount)
		if err != nil || u == nil || u.Total == 0 {
			if err != nil && logx.Verbose() {
				logx.Debugf("挂载点 %s 用量查询失败: %v", mount, err)
			}
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

// collectGPUsSync 同步采集本机 GPU：优先 NVML，失败回退 nvidia-smi。
// 由 gpuSampler 在独立 goroutine 里调用，超时预算脱离采集轮次的 interval。
//
// Windows 发行包是 no_nvml 构建（交叉编译无法带 CGO），所以那边实际上
// 100% 走 nvidia-smi 这条路，任何一环失败都会让整块 GPU 数据消失 ——
// 因此这里的每种失败都要留下可诊断的日志，而不是静默返回 nil。
func (l *Local) collectGPUsSync(ctx context.Context) ([]model.GPU, error) {
	// 优先尝试 NVML（快 4-5 倍：5-15ms vs 50-200ms）
	if gpus := l.collectGPUsNVML(ctx); len(gpus) > 0 {
		return gpus, nil
	}

	// 启动时没探到路径（常见于 Windows 服务：服务进程的 PATH 不含驱动目录，
	// 或安装时驱动还没就绪），按 30s 退避重试探测 —— 避免每轮重跑完整扫描。
	if l.nvsmi == "" {
		if time.Since(l.nvsmiProbedAt) < nvsmiReprobeInterval {
			return nil, nil // 退避期内，跳过本轮探测
		}
		l.nvsmiProbedAt = time.Now()
		l.nvsmi = defaultNvidiaSmiPath()
		if l.nvsmi == "" {
			l.warnGPUOnce("找不到 nvidia-smi，GPU 数据不可用（可在配置里显式设置 nvidia_smi 路径）")
			return nil, nil
		}
		logx.Infof("node %q: 已定位 nvidia-smi: %s", l.name, l.nvsmi)
	}

	start := time.Now()
	res := runNvidiaSmi(ctx, l.nvsmi, nvidiaArgs())
	if logx.Verbose() {
		logx.Debugf("node %q: nvidia-smi 耗时 %dms, stdout %d bytes",
			l.name, time.Since(start).Milliseconds(), len(res.stdout))
	}

	gpus := parseNvidiaCSV(res.stdout)
	if len(gpus) > 0 {
		l.gpuWarned = "" // 恢复正常，允许下次故障重新告警
		return gpus, nil
	}

	if res.err != nil {
		// 路径可能因驱动升级而失效（DriverStore 目录名带版本号），
		// 清空以便重新探测（30s 退避限制探测频率）。
		if !isFile(l.nvsmi) {
			logx.Infof("node %q: nvidia-smi 路径已失效（%s），将重新探测", l.name, l.nvsmi)
			l.nvsmi = ""
			l.nvsmiProbedAt = time.Time{} // 立即允许重探
		}
		l.warnGPUOnce("nvidia-smi 执行失败: " + res.briefError())
		return nil, res.err
	}
	l.warnGPUOnce("nvidia-smi 未返回可解析的 GPU 数据: " + res.briefError())
	return nil, nil
}

// warnGPUOnce 打印 GPU 采集告警，相同内容只打一次。
// 采集是秒级循环，若不去重，一台没有 N 卡的机器会把日志刷爆。
func (l *Local) warnGPUOnce(msg string) {
	if l.gpuWarned == msg {
		return
	}
	l.gpuWarned = msg
	logx.Infof("node %q: %s", l.name, msg)
}

func (l *Local) collectBlockDevices(ctx context.Context) []model.Disk {
	if runtime.GOOS == "windows" {
		// 原生 IOCTL 枚举，取代原来的 wmic / PowerShell 子进程方案
		out, err := collectBlockDevicesWindows(ctx, l.diskFilter)
		if err != nil {
			logx.Infof("node %q: Windows 物理磁盘枚举失败: %v", l.name, err)
			return nil
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Device < out[j].Device })
		return out
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
