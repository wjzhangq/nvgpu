package collector

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wjzhangq/gpumon/internal/model"
)

// TestGPUSamplerSingleFlight 验证并发调用只触发一次真实采集。
func TestGPUSamplerSingleFlight(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	collectFn := func(ctx context.Context) ([]model.GPU, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return []model.GPU{{Index: 0, Model: "Test GPU"}}, nil
	}

	s := newGPUSampler("test", 1*time.Second, 5*time.Second, collectFn)

	// 并发触发 10 次刷新
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.maybeRefresh()
		}()
	}
	wg.Wait()

	time.Sleep(100 * time.Millisecond) // 等待异步采集完成

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("期望单飞只调用 1 次 collectFn，实际调用了 %d 次", count)
	}

	gpus, _ := s.get()
	if len(gpus) != 1 {
		t.Errorf("期望缓存 1 张 GPU，实际 %d 张", len(gpus))
	}
}

// TestGPUSamplerCacheHit 验证缓存命中时不重复采集。
func TestGPUSamplerCacheHit(t *testing.T) {
	var callCount int
	var mu sync.Mutex

	collectFn := func(ctx context.Context) ([]model.GPU, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		return []model.GPU{{Index: 0}}, nil
	}

	s := newGPUSampler("test", 1*time.Second, 5*time.Second, collectFn)
	s.warmup()
	time.Sleep(100 * time.Millisecond)

	// 缓存未过期（< 1s），多次 get 不应触发新采集
	for i := 0; i < 5; i++ {
		s.get()
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	count := callCount
	mu.Unlock()

	if count > 2 {
		t.Errorf("缓存命中时不应重复采集，期望 ≤2 次，实际 %d 次", count)
	}
}

// TestGPUSamplerStaleExpiry 验证缓存超过过期阈值后返回 nil。
func TestGPUSamplerStaleExpiry(t *testing.T) {
	collectFn := func(ctx context.Context) ([]model.GPU, error) {
		// 永远阻塞，模拟采集失败导致缓存无法更新
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s := newGPUSampler("test", 100*time.Millisecond, 10*time.Millisecond, collectFn)

	// 手动设置一个旧缓存
	s.mu.Lock()
	s.gpus = []model.GPU{{Index: 0}}
	s.sampledAt = time.Now().Add(-1 * time.Minute) // 1 分钟前
	s.mu.Unlock()

	gpus, _ := s.get()
	if gpus != nil {
		t.Errorf("缓存超过过期阈值（max(3×interval, 30s)）应返回 nil，实际返回了 %d 张", len(gpus))
	}
}

// TestGPUSamplerTimeoutBackoff 验证连续超时计数递增。
func TestGPUSamplerTimeoutBackoff(t *testing.T) {
	collectFn := func(ctx context.Context) ([]model.GPU, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s := newGPUSampler("test", 1*time.Second, 50*time.Millisecond, collectFn)

	// 触发 3 次超时
	for i := 0; i < 3; i++ {
		s.maybeRefresh()
		time.Sleep(100 * time.Millisecond)
	}

	count := s.consecutiveTimeouts()
	if count != 3 {
		t.Errorf("期望连续超时计数为 3，实际 %d", count)
	}

	// 成功一次后应清零
	s.mu.Lock()
	s.collectFn = func(ctx context.Context) ([]model.GPU, error) {
		return []model.GPU{{Index: 0}}, nil
	}
	s.mu.Unlock()
	s.maybeRefresh()
	time.Sleep(100 * time.Millisecond)

	count = s.consecutiveTimeouts()
	if count != 0 {
		t.Errorf("成功采集后超时计数应清零，实际 %d", count)
	}
}

// TestGPUSamplerPreservesStaleOnError 验证采集失败时保留旧值。
func TestGPUSamplerPreservesStaleOnError(t *testing.T) {
	firstCall := true
	collectFn := func(ctx context.Context) ([]model.GPU, error) {
		if firstCall {
			firstCall = false
			return []model.GPU{{Index: 0, Model: "GPU0"}}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	s := newGPUSampler("test", 100*time.Millisecond, 50*time.Millisecond, collectFn)
	s.warmup()
	time.Sleep(100 * time.Millisecond)

	// 首次应有数据
	gpus, _ := s.get()
	if len(gpus) != 1 || gpus[0].Model != "GPU0" {
		t.Fatalf("预热后应有 1 张 GPU，实际 %d 张", len(gpus))
	}

	// 触发第二次采集（会超时失败）
	time.Sleep(150 * time.Millisecond)
	s.maybeRefresh()
	time.Sleep(100 * time.Millisecond)

	// 失败后应仍返回旧值
	gpus, sampledAt := s.get()
	if len(gpus) != 1 || gpus[0].Model != "GPU0" {
		t.Errorf("采集失败应保留旧值，期望 GPU0，实际 %d 张", len(gpus))
	}

	// sampledAt 不应更新（仍然是首次成功时的时刻）
	age := time.Since(sampledAt)
	if age < 150*time.Millisecond {
		t.Errorf("失败后 sampledAt 不应更新，年龄应 ≥150ms，实际 %dms", age.Milliseconds())
	}
}
