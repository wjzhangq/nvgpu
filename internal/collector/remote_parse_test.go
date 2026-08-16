package collector

import (
	"strings"
	"testing"

	"github.com/wjzhangq/gpumon/internal/model"
)

func TestSplitSections(t *testing.T) {
	out := "#=HOST\nhostname=box\n#=MEM\nMemTotal: 1 kB\n#=END\n"
	sec := splitSections(out)

	if got := sec["HOST"]; len(got) != 1 || got[0] != "hostname=box" {
		t.Fatalf("HOST 段解析错误: %#v", got)
	}
	if _, ok := sec["END"]; !ok {
		t.Fatal("缺少 END 结束标记")
	}
}

func TestParseMeminfoWithMemAvailable(t *testing.T) {
	lines := []string{
		"MemTotal:       131072000 kB",
		"MemFree:         10000000 kB",
		"MemAvailable:   100000000 kB",
	}
	m := parseMeminfo(lines)

	if m.TotalBytes != 131072000*1024 {
		t.Fatalf("total 错误: %d", m.TotalBytes)
	}
	if m.AvailableBytes != 100000000*1024 {
		t.Fatalf("available 错误: %d", m.AvailableBytes)
	}
	if m.UsedBytes != (131072000-100000000)*1024 {
		t.Fatalf("used 错误: %d", m.UsedBytes)
	}
	if m.UsagePercent < 23 || m.UsagePercent > 25 {
		t.Fatalf("usage 百分比不合理: %v", m.UsagePercent)
	}
}

func TestParseMeminfoFallback(t *testing.T) {
	// 没有 MemAvailable 时走经典估算
	lines := []string{
		"MemTotal:        1000000 kB",
		"MemFree:          200000 kB",
		"Buffers:           50000 kB",
		"Cached:           150000 kB",
		"SReclaimable:      10000 kB",
		"Shmem:             10000 kB",
	}
	m := parseMeminfo(lines)
	want := uint64(200000+50000+150000+10000-10000) * 1024
	if m.AvailableBytes != want {
		t.Fatalf("available 回退估算错误: got %d want %d", m.AvailableBytes, want)
	}
}

func TestParseDFTyped(t *testing.T) {
	lines := []string{
		"Filesystem     Type     1B-blocks         Used    Available Capacity Mounted on",
		"/dev/nvme0n1p2 ext4   2000000000000 500000000000 1400000000000      27% /",
		"tmpfs          tmpfs     8000000000            0    8000000000       0% /run",
		"/dev/nvme1n1   xfs    4000000000000 100000000000 3900000000000       3% /data",
	}
	disks := parseDF(lines, nil)

	if len(disks) != 2 {
		t.Fatalf("应过滤掉 tmpfs，得到 %d 条: %#v", len(disks), disks)
	}
	if disks[0].Mount != "/" || disks[0].FSType != "ext4" {
		t.Fatalf("第一条解析错误: %#v", disks[0])
	}
	if disks[0].TotalBytes != 2000000000000 {
		t.Fatalf("total 错误: %d", disks[0].TotalBytes)
	}
	if disks[1].Mount != "/data" {
		t.Fatalf("排序或过滤错误: %#v", disks[1])
	}
}

func TestParseDFUntypedKilobytes(t *testing.T) {
	lines := []string{
		"Filesystem     1024-blocks      Used Available Capacity Mounted on",
		"/dev/sda1         20000000   5000000  15000000      25% /",
	}
	disks := parseDF(lines, nil)
	if len(disks) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(disks))
	}
	if disks[0].TotalBytes != 20000000*1024 {
		t.Fatalf("1K 块换算错误: %d", disks[0].TotalBytes)
	}
}

func TestParseDFWhitelistKeepsSmallMounts(t *testing.T) {
	lines := []string{
		"Filesystem     Type   1B-blocks       Used  Available Capacity Mounted on",
		"tmpfs          tmpfs  1000000000  400000000  600000000      40% /mnt/ram",
	}
	// 白名单里的挂载点即使是 tmpfs、即使小于 1GiB 也要保留
	disks := parseDF(lines, map[string]bool{"/mnt/ram": true})
	if len(disks) != 1 || disks[0].Mount != "/mnt/ram" {
		t.Fatalf("白名单未生效: %#v", disks)
	}
}

func TestParseProcStatAndUsage(t *testing.T) {
	prev := parseProcStat([]string{"cpu0 100 0 100 800 0 0 0 0 0 0"})
	cur := parseProcStat([]string{"cpu0 200 0 200 1600 0 0 0 0 0 0"})

	u, ok := usageBetween(prev["cpu0"], cur["cpu0"])
	if !ok {
		t.Fatal("应能算出使用率")
	}
	// 增量 total=1000, idle=800 → 20%
	if u < 19.9 || u > 20.1 {
		t.Fatalf("使用率错误: %v", u)
	}
}

func TestUsageBetweenRejectsCounterReset(t *testing.T) {
	prev := parseProcStat([]string{"cpu0 1000 0 1000 8000 0 0 0 0 0 0"})
	cur := parseProcStat([]string{"cpu0 10 0 10 80 0 0 0 0 0 0"})
	if _, ok := usageBetween(prev["cpu0"], cur["cpu0"]); ok {
		t.Fatal("计数器回绕时不应产出数值")
	}
}

func TestParseProcCPUInfoX86DualSocket(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 4; i++ {
		phys := 0
		if i >= 2 {
			phys = 1
		}
		b.WriteString("processor\t: " + itoa(i) + "\n")
		b.WriteString("model name\t: Intel(R) Xeon(R) w7-2495X\n")
		b.WriteString("physical id\t: " + itoa(phys) + "\n")
		b.WriteString("core id\t: " + itoa(i%2) + "\n\n")
	}

	entries := parseProcCPUInfo(strings.Split(b.String(), "\n"))
	if len(entries) != 4 {
		t.Fatalf("应解析出 4 个逻辑核，得到 %d", len(entries))
	}

	topo := buildRemoteTopology(entries, map[string]string{}, model.HostInfo{Arch: "x86_64"})
	if len(topo) != 2 {
		t.Fatalf("应识别出 2 路 CPU，得到 %d", len(topo))
	}
	if len(topo[0].logical) != 2 || topo[0].logical[0] != 0 {
		t.Fatalf("socket 0 的逻辑核映射错误: %#v", topo[0].logical)
	}
	if topo[1].logical[0] != 2 {
		t.Fatalf("socket 1 的逻辑核映射错误: %#v", topo[1].logical)
	}
	if topo[0].model == "" {
		t.Fatal("CPU 型号不应为空")
	}
}

func TestBuildRemoteTopologyAarch64FallsBackToLscpu(t *testing.T) {
	// aarch64 的 /proc/cpuinfo 没有 model name / physical id / core id
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("processor\t: " + itoa(i) + "\n")
		b.WriteString("BogoMIPS\t: 2000.00\n\n")
	}
	entries := parseProcCPUInfo(strings.Split(b.String(), "\n"))

	lscpu := map[string]string{
		"model name":         "Cortex-A725",
		"socket(s)":          "1",
		"core(s) per socket": "20",
		"cpu(s)":             "20",
	}
	topo := buildRemoteTopology(entries, lscpu, model.HostInfo{Arch: "aarch64", Model: "NVIDIA DGX Spark"})

	if len(topo) != 1 {
		t.Fatalf("应为单路，得到 %d", len(topo))
	}
	if topo[0].model != "Cortex-A725" {
		t.Fatalf("型号应回退到 lscpu: %q", topo[0].model)
	}
	if topo[0].physCores != 20 || len(topo[0].logical) != 20 {
		t.Fatalf("核数错误: phys=%d logical=%d", topo[0].physCores, len(topo[0].logical))
	}
}

func TestParseNvidiaCSV(t *testing.T) {
	out := "0, NVIDIA RTX PRO 5000 Blackwell, GPU-abc, 37, 49140, 12288\n" +
		"1, NVIDIA RTX PRO 5000 Blackwell, GPU-def, 0, 49140, 0\n"
	gpus := parseNvidiaCSV(out)

	if len(gpus) != 2 {
		t.Fatalf("应解析出 2 张卡，得到 %d", len(gpus))
	}
	if gpus[0].VRAMTotalBytes != 49140*1024*1024 {
		t.Fatalf("显存换算错误: %d", gpus[0].VRAMTotalBytes)
	}
	if gpus[0].VRAMUsagePercent < 24 || gpus[0].VRAMUsagePercent > 26 {
		t.Fatalf("显存占用百分比错误: %v", gpus[0].VRAMUsagePercent)
	}
	if gpus[1].UtilizationPercent != 0 {
		t.Fatalf("利用率错误: %v", gpus[1].UtilizationPercent)
	}
}

func TestParseNvidiaCSVSkipsGarbage(t *testing.T) {
	out := "NVIDIA-SMI has failed because it couldn't communicate with the driver\n"
	if gpus := parseNvidiaCSV(out); len(gpus) != 0 {
		t.Fatalf("驱动报错文本不应被当成 GPU: %#v", gpus)
	}
}

func TestDetectUnifiedMemory(t *testing.T) {
	// GB10：128GB 统一内存，nvidia-smi 报出来的显存和系统内存接近
	ram := uint64(128) << 30
	gpus := []model.GPU{{VRAMTotalBytes: uint64(119) << 30}}
	if !model.DetectUnifiedMemory("aarch64", ram, gpus) {
		t.Fatal("GB10 应被识别为统一内存")
	}

	// x86 独显工作站：不应误判
	if model.DetectUnifiedMemory("x86_64", uint64(512)<<30, []model.GPU{{VRAMTotalBytes: uint64(48) << 30}}) {
		t.Fatal("x86 独显不应被识别为统一内存")
	}

	// arm64 但显存远小于内存（例如挂了张小卡）
	if model.DetectUnifiedMemory("aarch64", uint64(256)<<30, []model.GPU{{VRAMTotalBytes: uint64(16) << 30}}) {
		t.Fatal("显存与内存差距过大时不应判为统一内存")
	}
}

func TestParseBlockDevices(t *testing.T) {
	// lsblk 输出格式示例
	lines := []string{
		"nvme0n1 2000398934016 disk Samsung SSD 980 PRO 2TB 0",
		"sda 1000204886016 disk WDC WD10EZEX-08M2NA0 0",
		"sdb 500107862016 disk KINGSTON SA400S37500G 0",
		"loop0 109051904 loop - -1",
		"dm-0 524288000 dm - -1",
		"sr0 1073741312 rom - -1",
	}

	result := parseBlockDevices(lines, nil)

	if len(result) != 3 {
		t.Fatalf("期望 3 块物理盘，得到 %d", len(result))
	}

	// 验证第一块盘
	if result[0].Device != "nvme0n1" {
		t.Errorf("盘 0 设备名错误: %s", result[0].Device)
	}
	if result[0].TotalBytes != 2000398934016 {
		t.Errorf("盘 0 容量错误: %d", result[0].TotalBytes)
	}
	if result[0].Model != "Samsung SSD 980 PRO 2TB" {
		t.Errorf("盘 0 型号错误: %s", result[0].Model)
	}
	if result[0].Rotational == nil || *result[0].Rotational != false {
		t.Errorf("盘 0 应该是 SSD")
	}

	// 验证虚拟设备被过滤
	for _, d := range result {
		if strings.HasPrefix(d.Device, "loop") || strings.HasPrefix(d.Device, "dm-") || d.Type == "rom" {
			t.Errorf("虚拟设备或光驱不应出现在结果中: %s", d.Device)
		}
	}

	// 测试白名单
	filter := map[string]bool{"sda": true}
	result = parseBlockDevices(lines, filter)
	if len(result) != 1 || result[0].Device != "sda" {
		t.Errorf("白名单过滤失败")
	}

	// 测试 /sys/block 回退格式（MODEL 用 - 占位）
	sysLines := []string{
		"nvme0n1 2000398934016 disk - 0",
		"sda 1000204886016 disk WDC-WD10EZEX 1",
	}
	result = parseBlockDevices(sysLines, nil)
	if len(result) != 2 {
		t.Fatalf("回退格式解析失败，期望 2 块盘，得到 %d", len(result))
	}
	if result[0].Model != "" {
		t.Errorf("回退格式 MODEL=- 应该被解析为空字符串")
	}
	if result[1].Model != "WDC-WD10EZEX" {
		t.Errorf("回退格式 MODEL 解析错误: %s", result[1].Model)
	}
	if result[1].Rotational == nil || *result[1].Rotational != true {
		t.Errorf("盘 1 应该是机械盘")
	}
}

func TestIsVirtualBlockDevice(t *testing.T) {
	cases := []struct {
		name   string
		expect bool
	}{
		{"loop0", true},
		{"loop123", true},
		{"dm-0", true},
		{"dm-10", true},
		{"md0", true},
		{"zram0", true},
		{"ram0", true},
		{"nbd0", true},
		{"sda", false},
		{"nvme0n1", false},
		{"vda", false},
	}

	for _, c := range cases {
		got := isVirtualBlockDevice(c.name)
		if got != c.expect {
			t.Errorf("isVirtualBlockDevice(%q) = %v, 期望 %v", c.name, got, c.expect)
		}
	}
}

func TestBuildDynamicScript(t *testing.T) {
	// 测试 mount 模式
	script := buildDynamicScript("nvidia-smi", "mount")
	if !strings.Contains(script, "df -P -T -B1") {
		t.Error("mount 模式应该包含 df 命令")
	}
	if strings.Contains(script, "lsblk") {
		t.Error("mount 模式不应该包含 lsblk")
	}

	// 测试 block 模式
	script = buildDynamicScript("nvidia-smi", "block")
	if !strings.Contains(script, "lsblk") {
		t.Error("block 模式应该包含 lsblk 命令")
	}
	if !strings.Contains(script, "/sys/block") {
		t.Error("block 模式应该包含 /sys/block 回退逻辑")
	}
	if strings.Contains(script, "df -P -T") {
		t.Error("block 模式不应该包含 df 命令")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
