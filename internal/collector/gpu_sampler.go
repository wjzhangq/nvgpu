package collector

import (
	"context"
	"sync"
	"time"

	"github.com/wjzhangq/gpumon/internal/logx"
	"github.com/wjzhangq/gpumon/internal/model"
)

// gpuSampler 把 GPU 采集从"每轮同步阻塞"改成"单飞异步 + 缓存读取"。
//
// 核心问题：Windows 上 nvidia-smi 冷启动（驱动加载、卡从低功耗态唤醒）
// 经常要 1-4s，而 config.CollectTimeout 会把采集超时钳到 min(collect_timeout, interval)。
// 2s 的轮次预算由 CPU/内存/磁盘/GPU 四个子系统共享，GPU 稳定超时。
//
// 解决：GPU 采集运行在独立 goroutine，用自己的 gpuTimeout（默认 10s），
// 不受节点轮次 ctx 限制。主采集循环直接读缓存值，旧值比空值更有用。
type gpuSampler struct {
	name       string
	interval   time.Duration // 节点采集间隔
	gpuTimeout time.Duration // GPU 单次采集超时
	collectFn  func(context.Context) ([]model.GPU, error)

	mu          sync.Mutex
	gpus        []model.GPU
	sampledAt   time.Time // 本次 gpus 的采样时刻
	inFlight    bool
	lastErr     string
	timeoutRuns int // 连续超时次数
}

// newGPUSampler 构造 GPU 异步采集器。
// collectFn 是真实的采集函数（调 nvidia-smi 或 NVML）。
func newGPUSampler(name string, interval, gpuTimeout time.Duration, fn func(context.Context) ([]model.GPU, error)) *gpuSampler {
	return &gpuSampler{
		name:       name,
		interval:   interval,
		gpuTimeout: gpuTimeout,
		collectFn:  fn,
	}
}

// warmup 启动时预热一次，首屏就有数据。非阻塞。
func (s *gpuSampler) warmup() {
	s.maybeRefresh()
}

// get 返回缓存的 GPU 数据。
// 若缓存年龄 > interval 且当前没有采集在跑，触发一次异步刷新（本轮返回旧值）。
// 若缓存超过 max(3×interval, 30s) 未更新，丢弃返回 nil（避免陈旧利用率误导）。
func (s *gpuSampler) get() ([]model.GPU, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	age := now.Sub(s.sampledAt)

	// 过期保护：缓存太旧则丢弃
	staleThreshold := 3 * s.interval
	if staleThreshold < 30*time.Second {
		staleThreshold = 30 * time.Second
	}
	if age > staleThreshold {
		return nil, time.Time{}
	}

	// 缓存仍新鲜但超过 interval：触发异步刷新
	if age > s.interval && !s.inFlight {
		s.inFlight = true
		go s.refresh()
	}

	return s.gpus, s.sampledAt
}

// maybeRefresh 在无缓存或缓存过期时触发一次刷新，有采集在跑时跳过。
func (s *gpuSampler) maybeRefresh() {
	s.mu.Lock()
	if s.inFlight {
		s.mu.Unlock()
		return
	}
	s.inFlight = true
	s.mu.Unlock()
	go s.refresh()
}

// refresh 是异步刷新的实际工作函数。
// 用独立的 gpuTimeout，不受节点轮次 ctx 限制。
func (s *gpuSampler) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), s.gpuTimeout)
	defer cancel()

	start := time.Now()
	gpus, err := s.collectFn(ctx)
	elapsed := time.Since(start)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.inFlight = false

	if err != nil {
		s.lastErr = err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			s.timeoutRuns++
			if logx.Verbose() {
				logx.Debugf("node %q: GPU 采集超时 (%dms)，连续超时 %d 次",
					s.name, elapsed.Milliseconds(), s.timeoutRuns)
			}
		} else {
			s.timeoutRuns = 0 // 非超时错误清零计数
		}
		// 保留旧值，不更新 sampledAt —— 下次 get 仍能读到旧值直到过期
		return
	}

	// 成功：更新缓存，清零错误计数
	s.gpus = gpus
	s.sampledAt = time.Now()
	s.lastErr = ""
	s.timeoutRuns = 0

	if logx.Verbose() {
		logx.Debugf("node %q: GPU 采集成功，%d 张卡，耗时 %dms",
			s.name, len(gpus), elapsed.Milliseconds())
	}
}

// consecutiveTimeouts 返回连续超时次数，供调用方决定是否触发路径重探测。
func (s *gpuSampler) consecutiveTimeouts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeoutRuns
}
