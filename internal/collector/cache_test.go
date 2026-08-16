package collector

import (
	"context"
	"testing"
	"time"

	"github.com/wjzhangq/gpumon/internal/config"
)

// ttlPtr 构造 config.Duration 指针，便于测试。
func ttlPtr(d time.Duration) *config.Duration {
	return &config.Duration{Duration: d}
}

func TestDiskPartitionCache(t *testing.T) {
	// 构造 TTL 为 100ms 的采集器
	l := NewLocal(config.Node{
		Name:              "test",
		DiskMode:          "mount",
		PartitionCacheTTL: ttlPtr(100 * time.Millisecond),
	})

	if l.partitionsCacheTTL != 100*time.Millisecond {
		t.Fatalf("TTL 应为 100ms，得到 %v", l.partitionsCacheTTL)
	}

	// 首次调用：应扫描
	disks1 := l.collectDisks(context.Background())
	time1 := l.partitionsCacheTime

	if time1.IsZero() {
		t.Fatal("首次调用后 partitionsCacheTime 不应为零")
	}
	if len(l.partitionsCache) == 0 {
		t.Skip("未检测到挂载点（可能在不支持的系统上）")
	}

	time.Sleep(50 * time.Millisecond)

	// 缓存未过期：应复用
	disks2 := l.collectDisks(context.Background())
	if !l.partitionsCacheTime.Equal(time1) {
		t.Fatal("缓存未过期时不应重新扫描")
	}
	if len(disks2) != len(disks1) {
		t.Errorf("缓存复用时磁盘数量应一致: %d vs %d", len(disks1), len(disks2))
	}

	time.Sleep(60 * time.Millisecond)

	// 缓存过期：应重新扫描
	disks3 := l.collectDisks(context.Background())
	if l.partitionsCacheTime.Equal(time1) {
		t.Fatal("缓存过期后应重新扫描")
	}
	if len(disks3) == 0 {
		t.Error("重新扫描后不应返回空列表")
	}

	t.Logf("缓存测试通过：首次 %d 个挂载点，复用成功，过期后重新扫描", len(disks1))
}

func TestDiskPartitionCacheDisabled(t *testing.T) {
	// TTL=0 应禁用缓存
	l := NewLocal(config.Node{
		Name:              "test",
		DiskMode:          "mount",
		PartitionCacheTTL: ttlPtr(0),
	})

	if l.partitionsCacheTTL != 0 {
		t.Fatalf("TTL=0 时应禁用缓存，得到 %v", l.partitionsCacheTTL)
	}

	// 每次调用都应重新扫描
	l.collectDisks(context.Background())
	time1 := l.partitionsCacheTime

	time.Sleep(10 * time.Millisecond)

	l.collectDisks(context.Background())
	time2 := l.partitionsCacheTime

	if time2.Equal(time1) {
		t.Fatal("TTL=0 时每次都应重新扫描")
	}
}

func TestDiskPartitionCacheDefaultTTL(t *testing.T) {
	// 未指定 PartitionCacheTTL 时应使用默认 60s
	l := NewLocal(config.Node{
		Name:     "test",
		DiskMode: "mount",
	})

	if l.partitionsCacheTTL != 60*time.Second {
		t.Fatalf("未指定 TTL 时应默认为 60s，得到 %v", l.partitionsCacheTTL)
	}
}
