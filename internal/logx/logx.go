// Package logx 提供最小的日志分级封装。
//
// 底层仍然使用标准库 log，保持现有的时间戳格式；只是根据 verbose 开关
// 决定哪些日志输出到 stdout。采集 goroutine 并发读取状态，所以用 atomic。
package logx

import (
	"fmt"
	"log"
	"sync/atomic"
)

var verbose atomic.Bool

// SetVerbose 设置是否输出详细日志。
// main 里根据 -v / -verbose 参数调用。
func SetVerbose(v bool) {
	verbose.Store(v)
}

// Verbose 返回当前是否开启详细日志。
// 供调用方在拼接昂贵字符串前提前判断，避免浪费。
func Verbose() bool {
	return verbose.Load()
}

// Infof 输出 INFO 级别的日志，始终写入 stdout。
func Infof(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// Debugf 输出 DEBUG 级别的日志，仅在 verbose 开启时写入 stdout。
// 前缀 "[debug] " 便于区分。
func Debugf(format string, args ...interface{}) {
	if !verbose.Load() {
		return
	}
	log.Printf("[debug] "+format, args...)
}

// Fatalf 输出 FATAL 级别日志并退出进程。
func Fatalf(format string, args ...interface{}) {
	log.Fatalf("fatal: "+format, args...)
}

// Printf 是 Infof 的别名，供已有代码快速迁移使用。
func Printf(format string, args ...interface{}) {
	log.Printf(format, args...)
}

// Sprintf 便于调用方在条件判断后构造消息。
func Sprintf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
