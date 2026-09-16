package llm

import (
	"testing"
	"time"
)

// TestAttemptDelaySequence 验证两种间隔模式：
//   - backoff：1×→2×→3×→6× 序列（之后封顶 6×）
//   - fixed：每次相同
func TestAttemptDelaySequence(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 6, Mode: "backoff", Backoff: time.Second}.normalize()

	want := []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 6 * time.Second, 6 * time.Second}
	for i, w := range want {
		attempt := i + 2 // 首次重试是第 2 次请求
		if got := p.attemptDelay(attempt); got != w {
			t.Errorf("backoff 第 %d 次重试间隔 = %v，期望 %v", attempt, got, w)
		}
	}

	fixed := RetryPolicy{MaxAttempts: 3, Mode: "fixed", Backoff: 2 * time.Second}.normalize()
	for attempt := 2; attempt <= 4; attempt++ {
		if got := fixed.attemptDelay(attempt); got != 2*time.Second {
			t.Errorf("fixed 模式每次都应是基础间隔，第 %d 次 = %v", attempt, got)
		}
	}
}

// TestRetryPolicyNormalizeMode 非法/空模式一律回退 backoff（防脏值）。
func TestRetryPolicyNormalizeMode(t *testing.T) {
	for _, mode := range []string{"", "exponential", "EXPONENTIAL", "random"} {
		p := RetryPolicy{Mode: mode}.normalize()
		if p.Mode != "backoff" {
			t.Errorf("模式 %q 应归一化为 backoff，实际 %q", mode, p.Mode)
		}
	}
	for _, mode := range []string{"fixed", "backoff", "FIXED", "Backoff"} {
		p := RetryPolicy{Mode: mode}.normalize()
		if p.Mode != "fixed" && p.Mode != "backoff" {
			t.Errorf("合法模式 %q 被错误归一化为 %q", mode, p.Mode)
		}
	}
}
