package server

import (
	"strings"
	"testing"
	"time"
)

// TestNotifySkipReason 守护「任务完成系统通知」的三重闸门，
// 并要求跳过时必须给出**可读的具体原因**（日志用它自证为什么没通知）。
func TestNotifySkipReason(t *testing.T) {
	now := time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local)
	throttle := 3 * time.Second
	recent := now.Add(-1 * time.Second) // 1s 前刚通知过（落在节流窗口内）
	old := now.Add(-10 * time.Second)   // 10s 前通知过（已过窗口）

	cases := []struct {
		name    string
		enabled bool
		known   bool
		hidden  bool
		last    time.Time
		skip    bool   // true = 应跳过
		part    string // 跳过原因里必须出现的关键词
	}{
		{"窗口可见时不打扰", true, true, false, time.Time{}, true, "前台可见"},
		{"窗口隐藏且首次通知", true, true, true, time.Time{}, false, ""},
		{"窗口隐藏但刚通知过（节流）", true, true, true, recent, true, "节流"},
		{"窗口隐藏且已过节流窗口", true, true, true, old, false, ""},
		{"配置关闭时一律不通知", false, true, true, time.Time{}, true, "notify.enabled=false"},
		{"配置关闭且窗口可见", false, true, false, time.Time{}, true, "notify.enabled=false"},
		{"前端未上报可见性时保持可用", true, false, true, time.Time{}, false, ""},
	}

	for _, c := range cases {
		reason := notifySkipReason(c.enabled, c.known, c.hidden, c.last, now, throttle)
		if c.skip && reason == "" {
			t.Errorf("%s: 期望跳过并给出原因，实际放行", c.name)
			continue
		}
		if !c.skip && reason != "" {
			t.Errorf("%s: 期望放行，实际被跳过（%s）", c.name, reason)
			continue
		}
		if c.skip && c.part != "" && !strings.Contains(reason, c.part) {
			t.Errorf("%s: 跳过原因 %q 未包含 %q", c.name, reason, c.part)
		}
		// shouldNotify 必须与 reason 保持一致（避免两套判定漂移）
		if got := shouldNotify(c.enabled, c.known, c.hidden, c.last, now, throttle); got == c.skip {
			t.Errorf("%s: shouldNotify=%v 与 skip=%v 不一致", c.name, got, c.skip)
		}
	}
}

// TestClipText 守护按字符截断（不能把多字节汉字截成半个）。
func TestClipText(t *testing.T) {
	if got := clipText("  hello  ", 10); got != "hello" {
		t.Errorf("clipText 未去空白: %q", got)
	}
	if got := clipText("abcdef", 3); got != "abc…" {
		t.Errorf("clipText 截断结果 = %q, want %q", got, "abc…")
	}
	// 中文：按字符截断，5 个汉字截 3 个 → 3 个汉字 + 省略号
	if got := clipText("一二三四五", 3); got != "一二三…" {
		t.Errorf("clipText 中文截断 = %q, want %q", got, "一二三…")
	}
	if got := clipText("短", 10); got != "短" {
		t.Errorf("clipText 短文本未原样返回: %q", got)
	}
	if got := clipText("任意", 0); got != "任意" {
		t.Errorf("clipText max<=0 应不截断: %q", got)
	}
}
