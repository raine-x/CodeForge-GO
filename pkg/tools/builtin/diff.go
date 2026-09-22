package builtin

import (
	"fmt"
	"strings"
)

// op 是 diff 中的一行操作：' ' 相等、'-' 删除、'+' 新增。
type op struct {
	kind byte
	text string
}

const diffContextLines = 3

// diffBudget 是 LCS 动态规划允许的最大单元格数（作用于**裁剪后的中间段**）。
// 超过就退化为按位置逐行比对，避免 DP 表把内存吃光。
//
// ⚠️ 这个阈值必须配合 diffOps 的「先剥公共前缀/后缀」使用：单看数字
// 4_000_000 = 2000²，如果直接拿整份文件去比，就等于「文件一超过 2000 行，
// 任何改动都按整份文件重写上报」—— 实测 2524 行的 index.html 只改一行，
// 界面却显示 +2524/-2520（2026-09-22 反馈）。
const diffBudget = 4_000_000

// UnifiedDiff 生成 oldText -> newText 的 Unified Diff。
// 无差异时返回空字符串。oldName/newName 用于 --- / +++ 头。
func UnifiedDiff(oldName, newName, oldText, newText string) string {
	ops := diffOps(splitLines(oldText), splitLines(newText))
	if allEqual(ops) {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldName, newName)

	changed := make([]int, 0, len(ops))
	for i, o := range ops {
		if o.kind != ' ' {
			changed = append(changed, i)
		}
	}

	n := len(ops)
	for i := 0; i < len(changed); {
		start := changed[i] - diffContextLines
		if start < 0 {
			start = 0
		}
		end := changed[i] + diffContextLines
		j := i + 1
		for j < len(changed) && changed[j]-diffContextLines <= end {
			end = changed[j] + diffContextLines
			j++
		}
		if end > n-1 {
			end = n - 1
		}
		writeHunk(&sb, ops, start, end)
		i = j
	}
	return sb.String()
}

// LineChurn 统计 oldText -> newText 的行级增删数（added, removed）。
//
// 为什么要单独算：编辑/写入工具的返回值原本只有字节数，模型看不到自己
// 一次改了多少行。「覆盖式写入」把 2200 行文件整体重写、实际只动 30 行时，
// 界面上报的是 +2200/-2170，而模型收到的却是「写入成功，34608 字节」——
// 缺少这个信号，模型就没有从全量重写回到精确替换的纠正机会。
//
// ⚠️ 必须与 UnifiedDiff 共用 diffOps：两边各写一套判定，就会出现
// 「界面显示 +1/-1、模型收到 +2520/-2520」这种自相矛盾（2026-09-22 实测）。
func LineChurn(oldText, newText string) (added, removed int) {
	for _, o := range diffOps(splitLines(oldText), splitLines(newText)) {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
}

// diffOps 是 UnifiedDiff 与 LineChurn 共用的**唯一** diff 核心。
//
// 算法：先剥掉公共前缀与公共后缀（线性扫描），只对中间段求差异，再拼回去。
//
// 为什么要先剥：LCS 是 O(n*m) 的，直接对整份文件跑，2500 行的文件就要开
// 2500×2500 的 DP 表。早期实现为此设了「整文件退化」的旁路，代价是
// 大文件的任何小改动都被报成整份文件重写。而真实的编辑几乎总是局部的：
// 剥掉前后缀之后中间段通常只有几行，既小到可以放心跑 LCS，也就不会再触发退化。
//
// 返回的 ops 覆盖 a、b 的完整行序列（' ' 为公共前后缀，'-'/'+' 为中间段的差异），
// 因此 writeHunk 可以直接按下标算行号。
func diffOps(a, b []string) []op {
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	// suffix 上限取 min(len(a), len(b)) - prefix，保证前后缀不重叠。
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix &&
		a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	ops := make([]op, 0, len(a)+len(b))
	for _, l := range a[:prefix] {
		ops = append(ops, op{' ', l})
	}
	ops = append(ops, middleOps(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	for _, l := range a[len(a)-suffix:] {
		ops = append(ops, op{' ', l})
	}
	return ops
}

// middleOps 求中间段的差异：够小就走 LCS，过大才退化为按位置逐行比对。
func middleOps(a, b []string) []op {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	if len(a)*len(b) > diffBudget {
		return positionalOps(a, b)
	}
	return lcsDiff(a, b)
}

// positionalOps 是超大中间段的兜底：按位置逐行比对。
//
// 只有「剥完公共前后缀后中间段仍然巨大」才会走到这里（例如整份文件被重排），
// 此时按位置比对已经足够接近真实改动，而且**绝不会再把整个文件算成改动**。
func positionalOps(a, b []string) []op {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	ops := make([]op, 0, len(a)+len(b))
	for i := 0; i < n; i++ {
		switch {
		case i >= len(b):
			ops = append(ops, op{'-', a[i]})
		case i >= len(a):
			ops = append(ops, op{'+', b[i]})
		case a[i] == b[i]:
			ops = append(ops, op{' ', a[i]})
		default:
			ops = append(ops, op{'-', a[i]}, op{'+', b[i]})
		}
	}
	return ops
}

// writeHunk 输出 [start, end] 区间内的差异块。
func writeHunk(sb *strings.Builder, ops []op, start, end int) {
	oldStart, newStart := 1, 1
	for k := 0; k < start; k++ {
		if ops[k].kind != '+' {
			oldStart++
		}
		if ops[k].kind != '-' {
			newStart++
		}
	}
	oldCount, newCount := 0, 0
	for k := start; k <= end; k++ {
		if ops[k].kind != '+' {
			oldCount++
		}
		if ops[k].kind != '-' {
			newCount++
		}
	}
	fmt.Fprintf(sb, "@@ -%d,%d +%d,%d @@\n", oldStart, oldCount, newStart, newCount)
	for k := start; k <= end; k++ {
		sb.WriteByte(ops[k].kind)
		sb.WriteString(ops[k].text)
		sb.WriteByte('\n')
	}
}

// lcsDiff 基于最长公共子序列计算行级差异。
func lcsDiff(a, b []string) []op {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	ops := make([]op, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	return ops
}

func allEqual(ops []op) bool {
	for _, o := range ops {
		if o.kind != ' ' {
			return false
		}
	}
	return true
}

// splitLines 按行拆分文本，兼容 CRLF，并丢弃末尾空行。
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
