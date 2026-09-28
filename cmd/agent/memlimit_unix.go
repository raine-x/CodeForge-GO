//go:build !windows

package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// totalPhysMem 返回物理内存字节数；拿不到返回 0。
//
// Linux / Android 读 /proc/meminfo 的 MemTotal。Android 也是 Linux 内核，
// 同一份实现覆盖 Termux。不支持 /proc 的平台（如 darwin）返回 0，
// 于是 applyMemoryLimit 不设上限 —— 沿用 Go 默认，而不是瞎猜。
func totalPhysMem() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024 // MemTotal 的单位是 kB
	}
	return 0
}
