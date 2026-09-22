package builtin

import (
	"strings"
	"testing"
)

func TestUnifiedDiffBasic(t *testing.T) {
	oldText := "line1\nline2\nline3\nline4\n"
	newText := "line1\nline2-changed\nline3\nline4\n"
	diff := UnifiedDiff("a.txt", "a.txt", oldText, newText)

	if diff == "" {
		t.Fatal("期望产生差异，实际为空")
	}
	if !strings.Contains(diff, "-line2") {
		t.Errorf("差异中缺少删除行: %s", diff)
	}
	if !strings.Contains(diff, "+line2-changed") {
		t.Errorf("差异中缺少新增行: %s", diff)
	}
	if !strings.Contains(diff, "--- a.txt") || !strings.Contains(diff, "+++ a.txt") {
		t.Errorf("差异缺少文件头: %s", diff)
	}
}

func TestUnifiedDiffNoChange(t *testing.T) {
	text := "same\ncontent\n"
	if diff := UnifiedDiff("a", "a", text, text); diff != "" {
		t.Fatalf("无差异时应返回空字符串，实际: %q", diff)
	}
}

func TestUnifiedDiffEmptyToContent(t *testing.T) {
	diff := UnifiedDiff("new.txt", "new.txt", "", "hello\n")
	if !strings.Contains(diff, "+hello") {
		t.Fatalf("新增文件差异不正确: %s", diff)
	}
}

func TestUnifiedDiffHunkHeader(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 20; i++ {
		oldLines = append(oldLines, "L"+itoa(i))
		newLines = append(newLines, "L"+itoa(i))
	}
	newLines[10] = "CHANGED"

	diff := UnifiedDiff("f", "f", strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n")
	if !strings.Contains(diff, "@@") {
		t.Fatalf("缺少 hunk 头: %s", diff)
	}
	if strings.Count(diff, "@@") != 2 {
		t.Errorf("期望单个 hunk（2 个 @@ 标记），实际: %s", diff)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}

// ---------------------------------------------------------------------------
// 大文件的行数统计：小改动不得被报成整份文件重写
//
// 2026-09-22 反馈：2524 行的 assets/www/index.html 只改一行，界面显示 +2524/-2520。
// 根因是「len(a)*len(b) > 4_000_000 就整文件退化」，而 4_000_000 = 2000²，
// 等于文件一超过 2000 行，任何改动都按整份文件上报。下面按行数把阈值两侧都钉住。
// ---------------------------------------------------------------------------

// countDiffStat 复刻「界面口径」的统计方式（逐行数 +/-，排除 ---/+++ 头）。
// 服务端对应的实现是 pkg/agent.previewFor：它把 UnifiedDiff 的输出逐行数一遍。
// 存在的意义：确保「界面显示的数字」与「回传给模型的行数」出自同一份 diff，不会互相矛盾。
func countDiffStat(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			removed++
		}
	}
	return added, removed
}

// bigFile 生成 n 行、每行内容互不相同的文件（保证公共前后缀只能剥到改动点为止）。
func bigFile(n int) string {
	var sb strings.Builder
	for i := 1; i <= n; i++ {
		sb.WriteString("line ")
		sb.WriteString(itoa(i))
		sb.WriteString(": some stable content here\n")
	}
	return sb.String()
}

// 阈值两侧都要正确：2000 行本来就没退化，2001 行起才是回归的重灾区。
func TestLineChurnLargeFileSmallEdit(t *testing.T) {
	for _, n := range []int{2000, 2001, 2520, 3000} {
		oldText := bigFile(n)
		newText := strings.Replace(oldText,
			"line 1: some stable content here\n",
			"line 1: CHANGED content here\n", 1)

		if added, removed := LineChurn(oldText, newText); added != 1 || removed != 1 {
			t.Errorf("%d 行文件改 1 行：LineChurn 应报 +1/-1，实际 +%d/-%d", n, added, removed)
		}
		uiA, uiR := countDiffStat(UnifiedDiff("f", "f", oldText, newText))
		if uiA != 1 || uiR != 1 {
			t.Errorf("%d 行文件改 1 行：界面应显示 +1/-1，实际 +%d/-%d", n, uiA, uiR)
		}
	}
}

// 复刻反馈里的真实形状：2520 行 -> 2524 行（净增 4 行）。
// 曾经会显示 +2524/-2520（= 整份文件行数），正确结果是只报真正变动的行。
func TestLineChurnLargeFileNetGrowth(t *testing.T) {
	const before, after = 2520, 2524
	oldText := bigFile(before)

	// 在文件靠前处把 1 行替换成 5 行：净增 4 行。
	replacement := "line 2: NEW A\nline 2: NEW B\nline 2: NEW C\nline 2: NEW D\nline 2: NEW E\n"
	newText := strings.Replace(oldText, "line 2: some stable content here\n", replacement, 1)
	if got := strings.Count(newText, "\n"); got != after {
		t.Fatalf("构造有误：新文件应为 %d 行，实际 %d", after, got)
	}

	added, removed := LineChurn(oldText, newText)
	if added != 5 || removed != 1 {
		t.Errorf("应报 +5/-1，实际 +%d/-%d", added, removed)
	}
	if uiA, uiR := countDiffStat(UnifiedDiff("f", "f", oldText, newText)); uiA != 5 || uiR != 1 {
		t.Errorf("界面应显示 +5/-1，实际 +%d/-%d", uiA, uiR)
	}
}

// 纯插入：曾经的按位置比对会把其后所有行都算成改动（3000 行插 1 行报 +2998/-2997）。
func TestLineChurnLargeFilePureInsertion(t *testing.T) {
	const n = 3000
	oldText := bigFile(n)
	newText := strings.Replace(oldText,
		"line 3: some stable content here\n",
		"line 3: some stable content here\nINSERTED BRAND NEW LINE\n", 1)

	if added, removed := LineChurn(oldText, newText); added != 1 || removed != 0 {
		t.Errorf("插入 1 行应报 +1/-0，实际 +%d/-%d", added, removed)
	}
	if uiA, uiR := countDiffStat(UnifiedDiff("f", "f", oldText, newText)); uiA != 1 || uiR != 0 {
		t.Errorf("界面应显示 +1/-0，实际 +%d/-%d", uiA, uiR)
	}
}

// 尾部追加/删除同样不该把前面的行算进来。
func TestLineChurnLargeFileTailAppend(t *testing.T) {
	const n = 2500
	oldText := bigFile(n)
	newText := oldText + "appended tail line\n"

	if added, removed := LineChurn(oldText, newText); added != 1 || removed != 0 {
		t.Errorf("尾部追加应报 +1/-0，实际 +%d/-%d", added, removed)
	}
	if added, removed := LineChurn(newText, oldText); added != 0 || removed != 1 {
		t.Errorf("尾部删除应报 +0/-1，实际 +%d/-%d", added, removed)
	}
}

// 两函数必须永远一致：界面数字与回传模型的行数出自同一份 diff 核心。
func TestLineChurnMatchesUnifiedDiff(t *testing.T) {
	const n = 2600
	base := bigFile(n)
	cases := map[string][2]string{
		"无改动":   {base, base},
		"首行改":   {base, strings.Replace(base, "line 1: some stable content here\n", "CHANGED\n", 1)},
		"末行改":   {base, strings.Replace(base, "line 2600: some stable content here\n", "CHANGED\n", 1)},
		"中间删一行": {base, strings.Replace(base, "line 1300: some stable content here\n", "", 1)},
		"空到有":   {"", base},
		"有到空":   {base, ""},
		"整份重写":  {base, strings.ReplaceAll(base, "some stable content here", "totally different payload")},
	}
	for name, tc := range cases {
		churnA, churnR := LineChurn(tc[0], tc[1])
		uiA, uiR := countDiffStat(UnifiedDiff("f", "f", tc[0], tc[1]))
		if churnA != uiA || churnR != uiR {
			t.Errorf("%s：LineChurn 报 +%d/-%d，界面报 +%d/-%d，两者必须一致",
				name, churnA, churnR, uiA, uiR)
		}
	}
}

// 中间段过大时走按位置兜底：必须能正常结束，且不会把「整份文件」当成改动
// （整份重写的真实改动行数就是 n，所以这里断言上界而不是具体值）。
func TestUnifiedDiffHugeRewriteTerminates(t *testing.T) {
	const n = 2600
	oldText := bigFile(n)
	// 每一行都变，且顺序倒过来 —— 公共前后缀为 0，中间段 = 整份文件。
	var sb strings.Builder
	for i := n; i >= 1; i-- {
		sb.WriteString("rewritten ")
		sb.WriteString(itoa(i))
		sb.WriteString("\n")
	}
	newText := sb.String()

	added, removed := LineChurn(oldText, newText)
	if added <= 0 || removed <= 0 {
		t.Fatalf("整份重写应报出增删行，实际 +%d/-%d", added, removed)
	}
	if added > n || removed > n {
		t.Errorf("增删行数不应超过文件行数 %d，实际 +%d/-%d", n, added, removed)
	}
	if diff := UnifiedDiff("f", "f", oldText, newText); !strings.Contains(diff, "@@") {
		t.Errorf("整份重写应产出 hunk 头，实际: %q", diff)
	}
}
