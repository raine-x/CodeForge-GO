package logx

import (
	"strings"
	"testing"
)

// dispWidth 是对齐的基础，必须按终端列数而不是字节数算。
func TestDispWidth(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"信息", 4, "两个汉字 = 4 列，不是 6 字节"},
		{"启动", 4, "同上"},
		{"abc", 3, "半角 1 字符 1 列"},
		{"", 0, "空串"},
		{"模型：Atria", 4 + 2 + 5, "汉字 4 列 + 全角冒号 2 列 + 5 半角（：是全角，占 2 列）"},
		{"a提示b", 2 + 4, "半角与汉字混排"},
		{"\u0301x", 1, "组合附加符号宽度为 0"},
	}
	for _, c := range cases {
		if got := dispWidth(c.in); got != c.want {
			t.Errorf("dispWidth(%q) = %d, 期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

func TestPad对齐(t *testing.T) {
	// 所有徽章都是 2 个汉字，补齐到 badgeWidth 后正文起始列必须一致，
	// 否则中文行与将来可能出现的半角徽章会错位。
	for _, badge := range []string{badgeInfo, badgeWarn, badgeError, badgeDebug, badgeStart, badgeRun} {
		got := pad(badge, badgeWidth)
		if w := dispWidth(got); w != badgeWidth {
			t.Errorf("pad(%q) 宽度 %d，期望 %d", badge, w, badgeWidth)
		}
	}
	// 已超宽时不应截断，也不应补负数个空格
	if got := pad("很长的徽章", badgeWidth); got != "很长的徽章" {
		t.Errorf("超宽输入被改动了: %q", got)
	}
}

// 徽章配色只覆盖有语义的级别；「信息」保持默认色，
// 否则绝大多数行都是绿色，等于没有重点。
func TestBadgeColor语义(t *testing.T) {
	color = true
	defer func() { color = false }()

	if got := paint(badgeColor(badgeError), "x"); got != ansiRed+"x"+ansiReset {
		t.Errorf("错误级应上红色，得到 %q", got)
	}
	if got := paint(badgeColor(badgeWarn), "x"); got != ansiYellow+"x"+ansiReset {
		t.Errorf("提示级应上黄色，得到 %q", got)
	}
	if got := paint(badgeColor(badgeDebug), "x"); got != ansiGray+"x"+ansiReset {
		t.Errorf("调试级应上暗灰，得到 %q", got)
	}
	if got := paint(badgeColor(badgeStart), "x"); got != ansiCyan+"x"+ansiReset {
		t.Errorf("启动级应上青色，得到 %q", got)
	}
	if got := paint(badgeColor(badgeInfo), "x"); got != "x" {
		t.Errorf("信息级应保持默认色，得到 %q", got)
	}
}

// 关闭颜色后必须输出纯文本：重定向到文件时不能出现 ^[ 字面量。
func TestPaint关闭颜色后无转义(t *testing.T) {
	color = false
	if got := paint(ansiRed, "错误"); strings.ContainsRune(got, 0x1b) {
		t.Errorf("关闭颜色后仍含转义序列: %q", got)
	}
}
