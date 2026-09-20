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

// UnifiedDiff 生成 oldText -> newText 的 Unified Diff。
// 无差异时返回空字符串。oldName/newName 用于 --- / +++ 头。
func UnifiedDiff(oldName, newName, oldText, newText string) string {
	a := splitLines(oldText)
	b := splitLines(newText)

	// 超大文件退化为整体替换，避免 LCS 内存爆炸。
	if len(a)*len(b) > 4_000_000 {
		return fallbackDiff(oldName, newName, a, b)
	}

	ops := lcsDiff(a, b)
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
func LineChurn(oldText, newText string) (added, removed int) {
	a, b := splitLines(oldText), splitLines(newText)
	// 与 UnifiedDiff 同一条退化线：LCS 是 O(n*m)，超大文件按位置逐行比对。
	if len(a)*len(b) > 4_000_000 {
		for i := 0; i < len(a) || i < len(b); i++ {
			switch {
			case i >= len(b):
				removed++
			case i >= len(a):
				added++
			case a[i] != b[i]:
				added++
				removed++
			}
		}
		return added, removed
	}
	for _, o := range lcsDiff(a, b) {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return added, removed
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

// fallbackDiff 在超大文件场景下退化为整块替换。
func fallbackDiff(oldName, newName string, a, b []string) string {
	if len(a) == len(b) {
		same := true
		for i := range a {
			if a[i] != b[i] {
				same = false
				break
			}
		}
		if same {
			return ""
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", oldName, newName)
	fmt.Fprintf(&sb, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, l := range a {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range b {
		sb.WriteString("+" + l + "\n")
	}
	return sb.String()
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
