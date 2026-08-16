// Command gpumon 是一个单进程的多机指标监控器。
//
// 它按配置文件纳管本机和若干台远程 Linux 机器（远程走无 agent SSH），
// 周期性采集 CPU / 内存 / GPU / 显存 / 磁盘，在内存里保留每台机器最近
// 60 个采样点，并通过只读 HTTP JSON 接口对外提供当前值与历史数据。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/wjzhangq/gpumon/internal/api"
	"github.com/wjzhangq/gpumon/internal/collector"
	"github.com/wjzhangq/gpumon/internal/config"
	"github.com/wjzhangq/gpumon/internal/model"
	"github.com/wjzhangq/gpumon/internal/store"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
var version = "1.1.0-dev"

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	listen := flag.String("listen", "", "覆盖配置文件里的 server.listen")
	showVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	// 检测服务子命令
	if len(flag.Args()) > 0 && flag.Args()[0] == "service" {
		runServiceCommand(flag.Args()[1:], *cfgPath, *listen)
		return
	}

	if *showVersion {
		fmt.Println("gpumon", version)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	if err := run(*cfgPath, *listen); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run(cfgPath, listenOverride string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if listenOverride != "" {
		cfg.Server.Listen = listenOverride
	}

	st := store.New(cfg.Defaults.HistorySize, cfg.NodeNames())

	collectors, err := buildCollectors(cfg)
	if err != nil {
		return err
	}
	defer func() {
		for _, c := range collectors {
			_ = c.Close()
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for i := range cfg.Nodes {
		n := cfg.Nodes[i]
		c := collectors[i]
		timeout := cfg.CollectTimeout(n)

		wg.Add(1)
		go func() {
			defer wg.Done()
			runNode(ctx, c, n.Interval.Duration, timeout, st)
		}()

		log.Printf("node %q: type=%s interval=%s timeout=%s", n.Name, n.Type, n.Interval.Duration, timeout)
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api.New(cfg, st).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("HTTP 监听 %s（历史窗口 %d 点）", cfg.Server.Listen, st.Size())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		stop()
		wg.Wait()
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case <-ctx.Done():
		log.Printf("收到退出信号，正在关闭…")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP 关闭时出错: %v", err)
	}
	wg.Wait()
	return nil
}

func buildCollectors(cfg *config.Config) ([]collector.Collector, error) {
	out := make([]collector.Collector, 0, len(cfg.Nodes))
	for _, n := range cfg.Nodes {
		switch n.Type {
		case config.TypeLocal:
			out = append(out, collector.NewLocal(n))
		case config.TypeSSH:
			c, err := collector.NewRemote(n)
			if err != nil {
				return nil, err
			}
			out = append(out, c)
		default:
			return nil, fmt.Errorf("节点 %s: 未知类型 %s", n.Name, n.Type)
		}
	}
	return out, nil
}

// runNode 是单个节点的采集循环。
//
// 每一轮无论成功失败都会往 store 里写一条记录：失败时写 online=false，
// 这样历史序列不会出现时间空洞，前端能画出明确的断点。
func runNode(ctx context.Context, c collector.Collector, interval, timeout time.Duration, st *store.Store) {
	collectOnce := func() {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		snap, err := c.Collect(cctx)
		if err != nil {
			if snap.Node == "" {
				snap = model.Snapshot{Node: c.Name(), Timestamp: time.Now()}
			}
			snap.Online = false
			if snap.Error == "" {
				snap.Error = err.Error()
			}
			log.Printf("node %q: 采集失败: %v", c.Name(), err)
		}
		st.Append(snap)
	}

	collectOnce()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collectOnce()
		}
	}
}
