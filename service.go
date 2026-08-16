package main

import (
	"log"
	"os"

	"github.com/kardianos/service"
)

// program 实现 service.Interface，包装主逻辑使其能作为系统服务运行。
type program struct {
	cfgPath string
	listen  string
	exit    chan struct{}
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) run() {
	if err := run(p.cfgPath, p.listen); err != nil {
		log.Printf("服务运行出错: %v", err)
	}
}

func (p *program) Stop(s service.Service) error {
	// 通过关闭 exit 通道触发优雅退出（实际退出由 run 里的 signal 处理）
	close(p.exit)
	return nil
}

// runServiceCommand 处理服务管理子命令。
func runServiceCommand(args []string, cfgPath, listen string) {
	if len(args) == 0 {
		log.Fatal("service 子命令需要参数: install|uninstall|start|stop|restart|status")
	}

	// 构造服务配置
	svcConfig := &service.Config{
		Name:        "nvgpu",
		DisplayName: "GPU Monitor Service",
		Description: "多机 GPU 指标监控服务（本机 + 远程 SSH）",
	}

	// 服务安装时，参数需要持久化到服务定义里
	if args[0] == "install" {
		exe, err := os.Executable()
		if err != nil {
			log.Fatalf("无法获取可执行文件路径: %v", err)
		}
		svcConfig.Executable = exe
		svcConfig.Arguments = []string{"-config", cfgPath}
		if listen != "" {
			svcConfig.Arguments = append(svcConfig.Arguments, "-listen", listen)
		}
	}

	prg := &program{
		cfgPath: cfgPath,
		listen:  listen,
		exit:    make(chan struct{}),
	}

	s, err := service.New(prg, svcConfig)
	if err != nil {
		log.Fatalf("创建服务失败: %v", err)
	}

	cmd := args[0]
	switch cmd {
	case "install":
		err = s.Install()
		if err == nil {
			log.Printf("服务 %s 已安装", svcConfig.Name)
			log.Printf("  配置文件: %s", cfgPath)
			log.Printf("  启动命令: systemctl start %s (Linux) / sc start %s (Windows)", svcConfig.Name, svcConfig.Name)
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
		log.Fatalf("未知的服务命令: %s（支持: install, uninstall, start, stop, restart, status）", cmd)
	}

	if err != nil {
		log.Fatalf("服务 %s 失败: %v", cmd, err)
	}
}
