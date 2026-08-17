package collector

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunNvidiaSmiCapturesStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX shell")
	}
	// 模拟驱动没起来：非零退出 + stderr 有诊断文本。
	res := runNvidiaSmi(context.Background(), "/bin/sh", []string{
		"-c", "echo 'NVIDIA-SMI has failed because it could not communicate with the driver' >&2; exit 9",
	})

	if res.err == nil {
		t.Fatal("非零退出应返回 error")
	}
	if !strings.Contains(res.stderr, "NVIDIA-SMI has failed") {
		t.Fatalf("stderr 未被捕获: %q", res.stderr)
	}
	if got := res.briefError(); !strings.Contains(got, "NVIDIA-SMI has failed") {
		t.Fatalf("briefError 应优先取 stderr 首行，得到 %q", got)
	}
}

func TestRunNvidiaSmiKeepsStdoutOnFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX shell")
	}
	// 部分驱动会同时输出可用数据并以非零码退出，这时 stdout 仍然要能解析。
	line := "0, RTX 4090, GPU-abc, 85, 24576, 8192, 67, 320.50"
	res := runNvidiaSmi(context.Background(), "/bin/sh", []string{
		"-c", "echo '" + line + "'; exit 2",
	})

	if res.err == nil {
		t.Fatal("期望非零退出")
	}
	gpus := parseNvidiaCSV(res.stdout)
	if len(gpus) != 1 {
		t.Fatalf("退出码非零时 stdout 仍应可解析，得到 %d 张卡", len(gpus))
	}
	if gpus[0].TemperatureCelsius != 67 {
		t.Fatalf("温度解析错误: %d", gpus[0].TemperatureCelsius)
	}
}

func TestBriefErrorFallsBackToExecError(t *testing.T) {
	res := runNvidiaSmi(context.Background(), filepath.Join(t.TempDir(), "nope"), nil)
	if res.err == nil {
		t.Fatal("不存在的可执行文件应报错")
	}
	if res.briefError() == "" {
		t.Fatal("briefError 不应为空")
	}
}

func TestIsFile(t *testing.T) {
	dir := t.TempDir()
	if isFile(dir) {
		t.Error("目录不应判为文件")
	}
	if isFile(filepath.Join(dir, "missing")) {
		t.Error("不存在的路径不应判为文件")
	}

	f := filepath.Join(dir, "real")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isFile(f) {
		t.Error("普通文件应判为文件")
	}
}
