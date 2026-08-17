package collector

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
)

// isFile 判断路径存在且是普通文件（不是目录）。
func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// nvsmiResult 是一次 nvidia-smi 调用的结果。
type nvsmiResult struct {
	stdout string
	stderr string
	err    error
}

// runNvidiaSmi 执行一次 nvidia-smi 并同时捕获 stdout / stderr。
//
// 与 cmd.Output() 的区别有两点，都是 Windows 上排障必需的：
//   - stderr 单独留存。驱动没起来时 nvidia-smi 会以非零码退出并把原因
//     （"NVIDIA-SMI has failed because it couldn't communicate with the
//     NVIDIA driver"）写到 stderr，这行文本是唯一有价值的诊断信息。
//   - 即使退出码非零也把 stdout 交还调用方。部分驱动对不认识的查询字段
//     报错，但仍会把认识的那几列打印出来。
func runNvidiaSmi(ctx context.Context, path string, args []string) nvsmiResult {
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	hideWindow(cmd) // Windows 服务里不要弹控制台窗口

	err := cmd.Run()
	return nvsmiResult{
		stdout: stdout.String(),
		stderr: strings.TrimSpace(stderr.String()),
		err:    err,
	}
}

// briefError 把 nvidia-smi 的失败压成一行，用于日志。
// 优先用 stderr 的首行（信息量最大），没有则退回 exec 的错误文本。
func (r nvsmiResult) briefError() string {
	for _, s := range []string{r.stderr, r.stdout} {
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
			if line != "" {
				return truncate(line, 200)
			}
		}
	}
	if r.err != nil {
		return r.err.Error()
	}
	return "无输出"
}
