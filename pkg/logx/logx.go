// Package logx 统一 CodeForge 的前台日志输出。
//
// 为什么不用标准库 log：它只有一种前缀格式（全时间戳），而本项目实际有
// 十几种手写前缀（`[ws]` `[run]` `[notify]` `提示：` …）混用，扫读时找不到
// 重点。logx 固定成三列：
//
//	时间戳  徽章  正文
//	16:38:13  信息  模型：Atria-Dawn-Preview（provider=openai）
//
// 时间戳规则：**首行与新会话**打完整日期（`06-01-02 15:04:05`），
// 其余只打时分秒（`15:04:05`）。完整日期宽度是 14 列、时分秒 8 列，
// 这里不做补齐 —— 有意让两类时间戳各自顶格，视觉上就能看出「这是一次
// 新的开始」而不是又一行续流。
//
// 颜色只在有语义的地方上：错误红、提示黄、调试暗灰、里程碑（启动/运行）
// 青色，信息级保持默认色（满屏绿色反而没有重点）。输出被重定向或设置了
// NO_COLOR 时自动关闭，不会把 ANSI 码写进文件。
package logx

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// 徽章文案。徽章即级别，所以只有这几个。
const (
	badgeInfo  = "信息"
	badgeWarn  = "提示"
	badgeError = "错误"
	badgeDebug = "调试"
	badgeStart = "启动" // 进程第一行，同时强制完整时间戳
	badgeRun   = "运行" // 新会话/新任务边界，同时强制完整时间戳
)

const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[2m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
	ansiGray   = "\x1b[90m"
)

// badgeWidth 是徽章列的显示宽度。「信息」「启动」都是 2 个汉字 = 4 列，
// 留 2 列作列间隙。正文因此在所有行上左对齐。
const badgeWidth = 6

var (
	mu    sync.Mutex
	first = true // 尚未输出过任何一行
	color bool   // 由平台文件在 init 里决定
)

// dispWidth 计算**显示宽度**（终端列数），用于列对齐。
// 中文、全角字符占 2 列；直接用 len() 会让含中文的徽章把正文顶歪，
// 这是日志「参差不齐」最常见的来源。
func dispWidth(s string) int {
	w := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, // 韩文字母
			r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
			r >= 0xAC00 && r <= 0xD7A3, // 韩文音节
			r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
			r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
			r >= 0xFF00 && r <= 0xFF60, // 全角字母数字
			r >= 0xFFE0 && r <= 0xFFE6, // 全角符号
			r == 0x3000:                // 表意空格
			w += 2
		case r >= 0x0300 && r <= 0x036F: // 组合附加符号，宽度 0
		default:
			w++
		}
	}
	return w
}

func pad(s string, width int) string {
	if n := width - dispWidth(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func paint(code, s string) string {
	if !color || code == "" {
		return s
	}
	return code + s + ansiReset
}

// badgeColor 只给有语义的颜色：红=错误、黄=提示、灰=调试、青=里程碑。
// 「信息」保持终端默认色 —— 绝大多数行都是它，染色等于没有重点。
func badgeColor(badge string) string {
	switch badge {
	case badgeError:
		return ansiRed
	case badgeWarn:
		return ansiYellow
	case badgeDebug:
		return ansiGray
	case badgeStart, badgeRun:
		return ansiCyan
	}
	return ""
}

// emit 输出一行。full 为真时用完整日期时间。
func emit(badge string, full bool, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)

	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	// 首行强制完整日期：让终端滚屏后仍能看出这一轮进程何时开始。
	forceFull := full || first
	first = false

	var ts string
	if forceFull {
		ts = now.Format("06-01-02 15:04:05")
	} else {
		ts = now.Format("15:04:05")
	}

	// 三列：时间戳 → 徽章 → 正文。徽章后接 2 个空格作为列间隙。
	line := paint(ansiDim, ts) + "  " +
		paint(badgeColor(badge), pad(badge, badgeWidth)) + "  " +
		msg

	fmt.Fprintln(os.Stderr, line)
}

// Infof 常规信息。
func Infof(format string, a ...any) { emit(badgeInfo, false, format, a...) }

// Warnf 需要用户注意但不致命：提示、告警、降级。
func Warnf(format string, a ...any) { emit(badgeWarn, false, format, a...) }

// Errorf 出错但进程继续。
func Errorf(format string, a ...any) { emit(badgeError, false, format, a...) }

// Debugf 逐轮调试细节。保留输出，但压暗以免与主线争夺注意力。
func Debugf(format string, a ...any) { emit(badgeDebug, false, format, a...) }

// Startupf 进程生命周期的里程碑。
//
// **不强制**完整时间戳：首行完整日期已由 emit 的 first 标志自动处理，
// 这里若再强制，启动那几行会连着印四次完整日期，正好违背「只有首行与
// 新会话才显示日期」的规则。它只负责拿到启动徽章（青色）。
func Startupf(format string, a ...any) { emit(badgeStart, false, format, a...) }

// Runf 新会话 / 新任务的边界，强制完整日期时间戳。
// 用途是让「第几句对话」在日志里一眼可辨。
func Runf(format string, a ...any) { emit(badgeRun, true, format, a...) }

// Fatalf 输出错误行后退出（等价标准库 log.Fatalf 的退出码 1）。
func Fatalf(format string, a ...any) {
	emit(badgeError, false, format, a...)
	os.Exit(1)
}
