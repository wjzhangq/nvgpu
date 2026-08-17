package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/kardianos/service"

	"github.com/wjzhangq/gpumon/internal/config"
	"github.com/wjzhangq/gpumon/internal/logx"
)

// program 实现 service.Interface，包装主逻辑使其能作为系统服务运行。
//
// Stop 必须真正停下 run —— 之前的实现只是关掉一个没人监听的 channel，
// 服务管理器等超时后强杀进程，HTTP 连接和采集轮次都不会走优雅退出。
// 现在改成 context 取消 + 等 done，语义和命令行 Ctrl-C 完全一致。
type program struct {
	cfgPath string
	listen  string

	cancel context.CancelFunc
	done   chan struct{}
}

func (p *program) Start(s service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})

	go func() {
		defer close(p.done)
		if err := run(ctx, p.cfgPath, p.listen); err != nil {
			log.Printf("服务运行出错: %v", err)
		}
	}()
	return nil
}

func (p *program) Stop(s service.Service) error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.done != nil {
		<-p.done
	}
	return nil
}

// runAsServiceIfNeeded 在进程由服务管理器拉起（非交互式）时接管控制流并返回 true。
//
// 这是 Windows 上必须的一步：SCM 启动的进程必须调用 StartServiceCtrlDispatcher
// 完成握手，否则服务会在 30 秒后被判定为"未响应启动请求"（错误 1053），
// 即便业务逻辑已经正常跑起来了。kardianos/service 的 s.Run() 负责这个握手。
//
// 交互式运行（用户在终端里敲命令）时返回 false，由调用方走普通 run 流程。
func runAsServiceIfNeeded(cfgPath, listen string) bool {
	if service.Interactive() {
		return false
	}

	svcConfig := &service.Config{
		Name:        "nvgpu",
		DisplayName: "GPU Monitor Service",
		Description: "多机 GPU 指标监控服务（本机 + 远程 SSH）",
	}
	prg := &program{cfgPath: cfgPath, listen: listen}
	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("创建服务失败: %v", err)
	}
	if err := s.Run(); err != nil {
		log.Fatalf("服务运行失败: %v", err)
	}
	return true
}

// runServiceCommand 处理服务管理子命令。
func runServiceCommand(args []string, cfgPath, listen string) {
	if len(args) == 0 {
		log.Fatal("service 子命令需要参数: install|uninstall|start|stop|restart|status")
	}

	svcConfig := &service.Config{
		Name:        "nvgpu",
		DisplayName: "GPU Monitor Service",
		Description: "多机 GPU 指标监控服务（本机 + 远程 SSH）",
	}

	// 服务安装时，参数需要持久化到服务定义里。
	//
	// 相对路径必须在这里就展开成绝对路径：服务进程的工作目录由服务管理器
	// 决定（Windows 上是 C:\Windows\System32），"config.yaml" 这种相对路径
	// 装完就一定读不到。
	if args[0] == "install" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("无法获取可执行文件路径: %v", err)
		}
		absExe, err := filepath.Abs(exe)
		if err != nil {
			log.Fatalf("无法解析可执行文件绝对路径: %v", err)
		}
		absCfg, err := filepath.Abs(cfgPath)
		if err != nil {
			log.Fatalf("无法解析配置文件绝对路径 %s: %v", cfgPath, err)
		}

		// 装之前先把配置读一遍。装完才发现配置有错，得靠翻服务日志才能看出来，
		// 而这里报错是直接打在用户终端上的。
		if _, err := config.Load(absCfg); err != nil {
			log.Fatalf("配置文件 %s 校验失败，已中止安装: %v", absCfg, err)
		}

		svcConfig.Executable = absExe
		svcConfig.WorkingDirectory = filepath.Dir(absExe)
		svcConfig.Arguments = []string{"-config", absCfg}
		if listen != "" {
			svcConfig.Arguments = append(svcConfig.Arguments, "-listen", listen)
		}
		// -v 跟着一起持久化：安装时开了详细日志，说明用户正在排查问题，
		// 服务跑起来后也该继续输出。
		if logx.Verbose() {
			svcConfig.Arguments = append(svcConfig.Arguments, "-v")
		}
		cfgPath = absCfg
	}

	prg := &program{cfgPath: cfgPath, listen: listen}

	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("创建服务失败: %v", err)
	}

	cmd := args[0]
	switch cmd {
	case "install":
		err = s.Install()
		if err == nil {
			log.Printf("服务 %s 已安装（未启动）", svcConfig.Name)
			log.Printf("  可执行文件: %s", svcConfig.Executable)
			log.Printf("  工作目录:   %s", svcConfig.WorkingDirectory)
			log.Printf("  启动参数:   %v", svcConfig.Arguments)
			log.Printf("  启动命令:   sc start %s (Windows) / systemctl start %s (Linux)",
				svcConfig.Name, svcConfig.Name)
		}
	case "uninstall":
		err = s.Uninstall()
		if err == nil {
			log.Printf("服务 %s 已卸载", svcConfig.Name)
		}
	case "start":
		err = s.Start()
		if err == nil {
			log.Printf("服务 %s 已启动", svcConfig.Name)
		}
	case "stop":
		err = s.Stop()
		if err == nil {
			log.Printf("服务 %s 已停止", svcConfig.Name)
		}
	case "restart":
		err = s.Restart()
		if err == nil {
			log.Printf("服务 %s 已重启", svcConfig.Name)
		}
	case "run":
		// 由服务管理器调用（Windows SCM / systemd）。也允许手工执行用于前台调试。
		if err = s.Run(); err != nil {
			log.Fatalf("服务运行失败: %v", err)
		}
		return
	case "status":
		status, statusErr := s.Status()
		if statusErr != nil {
			log.Printf("无法查询服务状态: %v", statusErr)
			return
		}
		statusStr := map[service.Status]string{
			service.StatusUnknown: "未知",
			service.StatusRunning: "运行中",
			service.StatusStopped: "已停止",
		}
		log.Printf("服务状态: %s (%d)", statusStr[status], status)
		return
	default:
		log.Fatalf("未知的服务命令: %s（支持: install, uninstall, start, stop, restart, status, run）", cmd)
	}

	if err != nil {
		log.Fatalf("服务 %s 失败: %v", cmd, err)
	}
}
