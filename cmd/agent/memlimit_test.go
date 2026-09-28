package main

import "testing"

// TestMemLimitForClamps 软上限要同时有下限与上限。
//
// 之前完全没有设 GOMEMLIMIT（搜遍全仓 GOMEMLIMIT/SetMemoryLimit/GOGC 零命中），
// 走 Go 默认 GOGC=100 —— 没有任何软边界。实测常驻约 25~36 MB，
// 但一旦某个会话把上下文堆爆，进程会先吃满物理内存再被 OOMKill，
// 连带把会话库、审计日志一起带走。
func TestMemLimitForClamps(t *testing.T) {
	const (
		mib = 1 << 20
		gib = 1 << 30
	)
	cases := []struct {
		name  string
		total uint64
		want  int64
		why   string
	}{
		{"16GB 机器", 16 * gib, 2 * gib, "大内存上不设软上限 → GC 太懒，失控时能吃满整机"},
		{"8GB 机器", 8 * gib, 2 * gib, "同上，撞上限"},
		{"4GB 机器", 4 * gib, 1 * gib, "25% = 1GiB"},
		{"2GB 机器", 2 * gib, 512 * mib, "25% = 512MiB"},
		{"1GB 机器", 1 * gib, 256 * mib, "撞下限"},
		{"512MB 机器", 512 * mib, 256 * mib, "极小机器也不能低于下限，否则无意义地疯狂 GC"},
		{"内存未知", 0, -1, "拿不到内存就**不要设**，宁可沿用默认也不瞎猜"},
	}
	for _, c := range cases {
		got := memLimitFor(c.total)
		if got != c.want {
			t.Errorf("%s：memLimitFor(%d) = %d，期望 %d（%s）", c.name, c.total, got, c.want, c.why)
		}
	}
}

// TestMemLimitBelowCurrentUsage 说明软上限与常驻的关系：上限远高于常驻时，
// 它不改变日常行为，只在失控时才起作用。
func TestMemLimitBelowCurrentUsage(t *testing.T) {
	const gib = 1 << 30
	// 实测：空闲 21MB / 16 次真实会话后 27MB / 500KB 输入峰值 36MB
	const measuredPeak = 40 << 20
	if got := memLimitFor(4 * gib); got <= measuredPeak {
		t.Errorf("4GB 机器的上限 %d 应远高于实测峰值 %d，否则会改变日常行为", got, measuredPeak)
	}
}
