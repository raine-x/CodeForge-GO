//go:build !windows

package logx

import (
	"os"
)

// detectTerminal 用「stderr 是否字符设备」判断是不是终端。
//
// 这个判据在 Linux 与 Android(Termux) 上都成立：真终端 /dev/pts/* 是字符设备，
// 重定向到文件或管道则是普通文件。Termux 里 stderr 接 pty，因此照常上色。
func detectTerminal() bool {
	fi, err := os.Stderr.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
