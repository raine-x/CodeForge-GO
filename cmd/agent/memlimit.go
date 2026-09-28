package main

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// 内存软上限参数。
//
// 之前完全不设 GOMEMLIMIT，走 Go 默认 GOGC=100 —— 堆翻倍才 GC，没有任何软边界。
// 实测常驻只有 25~36 MB，日常不会碰到软上限；它的作用是**兜底**：某个会话把
// 上下文堆爆时，进程会在撞到物理内存上限被 OOMKill 之前先自己把内存压下来。
// 被 OOMKill 会连带丢掉未落盘的会话状态与正在写的审计日志。
const (
	// memLimitRatio 是软上限占物理内存的比例。
	//
	// 25% 的理由：Agent 的真实常驻（实测 25~36 MB）远低于任何机器的这个比例，
	// 所以设了不影响日常；真正需要它兜底的是「失控」场景，此时宁可多 GC 几次。
	memLimitRatio = 0.25
	// memLimitMin / memLimitMax 给上下限，避免两端的荒唐结果：
	//   - 下限：极小机器（512MB 的老笔记本 / Termux 设备）上 25% 只有 128MB，
	//     比常驻还紧，会让 GC 无意义地高频触发。
	//   - 上限：64GB 机器上 25% 是 16GB，等于没有约束，GC 该触发时仍然很懒。
	memLimitMin = 256 << 20 // 256 MiB
	memLimitMax = 2 << 30   // 2 GiB
	// memLimitEnv 供用户在特殊环境（超小容器 / 共享内存机器）覆盖。
	// 纯数字按字节；带单位（MiB/GiB）也可；0 或负数表示关闭。
	memLimitEnv = "CODEFORGE_GOMEMLIMIT"
)

// memLimitFor 按物理内存算出软上限，返回 -1 表示「不该设」。
//
// 抽成纯函数是为了能测边界：读物理内存涉及平台 API，测不了；
// 而「比例 + 上下限夹逼」这段逻辑恰恰是最容易写错的。
func memLimitFor(totalBytes uint64) int64 {
	if totalBytes == 0 {
		return -1 // 拿不到就**不要设**：宁可沿用 Go 默认，也不瞎猜一个值
	}
	limit := int64(float64(totalBytes) * memLimitRatio)
	if limit < memLimitMin {
		limit = memLimitMin
	}
	if limit > memLimitMax {
		limit = memLimitMax
	}
	return limit
}

// applyMemoryLimit 在启动早期设置内存软上限。
//
// 必须尽早、且在任何大分配之前 —— 主 Agent 初始化会建工具注册表、SQLite 连接。
// 放在 main() 最开头就是这个原因。
func applyMemoryLimit() {
	// 环境变量优先
	if v := strings.TrimSpace(os.Getenv(memLimitEnv)); v != "" {
		if n, err := parseByteSize(v); err == nil {
			if n <= 0 {
				debug.SetMemoryLimit(-1) // 显式关闭
			} else {
				debug.SetMemoryLimit(n)
			}
			return
		}
		// 解析失败就往下走自动逻辑，不因为一个手滑的环境变量让整个启动失败
	}
	if limit := memLimitFor(totalPhysMem()); limit > 0 {
		debug.SetMemoryLimit(limit)
	}
}

// parseByteSize 解析 "2GiB" / "512MiB" / "1073741824" 这类写法。
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	upper := strings.ToUpper(s)
	switch {
	case strings.HasSuffix(upper, "GIB"):
		mult, s = 1<<30, s[:len(s)-3]
	case strings.HasSuffix(upper, "MIB"):
		mult, s = 1<<20, s[:len(s)-3]
	case strings.HasSuffix(upper, "KIB"):
		mult, s = 1<<10, s[:len(s)-3]
	case strings.HasSuffix(upper, "GB"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(upper, "MB"):
		mult, s = 1<<20, s[:len(s)-2]
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
