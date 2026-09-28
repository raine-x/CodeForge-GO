//go:build windows

package logx

import (
	"os"

	"golang.org/x/sys/windows"
)

// detectTerminal 有两件事，缺一不可：
//
//  1. **打开 VT**。Windows 控制台默认不解释 ANSI 转义，不显式打开的话写出去
//     是一串 ^[ 字面量。ENABLE_VIRTUAL_TERMINAL_PROCESSING 是 Win10 1511
//     引入的，更老的系统会失败 —— 那就退回无颜色，不影响功能。
//  2. **判断是不是控制台**。GetConsoleMode 在重定向到文件/管道时返回错误，
//     这正是我们要的判据：重定向 → 不上色，避免 ANSI 码污染日志文件。
func detectTerminal() bool {
	console := false
	for _, f := range []*os.File{os.Stdout, os.Stderr} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(h, &mode); err != nil {
			continue // 不是控制台
		}
		console = true
		_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
	}
	return console
}
