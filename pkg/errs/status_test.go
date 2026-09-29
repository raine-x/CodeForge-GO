package errs

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// HTTP 状态码可达性。
//
// 改造前 `Classify` 只有三层判定：哨兵错误 → 网络错误 → 系统调用 →
// 证书 → 格式 → **字符串兜底**。上游状态码在 pkg/llm/sse.go 里被拍平成
// `fmt.Errorf("LLM 请求失败 (%d): %s", 429, body)`，没有任何结构化字段，
// 于是 `Classify` 只能去猜 body 文本。
//
// 而实测 GLM 的 429 body 是：
//
//	{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}
//
// 一个 "rate limit" / "too many requests" 都没有 ⇒ 掉进 KindUnknown。
// 401 同理（`"invalid api key"` 带下划线，匹配不上 "invalid api key"）。
//
// 后果：限流不触发退避重试建议、鉴权失败不触发「去检查密钥」的建议，
// 用户在界面上看到的是裸 JSON。

// TestClassifyUsesHTTPStatusCode 核心用例：状态码比 body 文本可靠。
//
// 用的是实测原文，body 里没有任何可猜的关键词。
func TestClassifyUsesHTTPStatusCode(t *testing.T) {
	rate := NewStatus(429, `{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}`, 0, nil)
	if got := Classify(rate); got != KindRateLimit {
		t.Errorf("HTTP 429 应归为限流，实际 %v", got)
	}
	if got := Classify(NewStatus(503, "No available channel", 0, nil)); got != KindUpstream {
		t.Errorf("HTTP 503 应归为上游故障，实际 %v", got)
	}
	// 空 body 的 401：字符串兜底完全无从下手，只有状态码能救
	if got := Classify(NewStatus(401, "", 0, nil)); got != KindAuth {
		t.Errorf("HTTP 401（空 body）应归为鉴权失败，实际 %v", got)
	}
	if got := Classify(NewStatus(403, `{"error":"forbidden"}`, 0, nil)); got != KindAuth {
		t.Errorf("HTTP 403 应归为鉴权失败，实际 %v", got)
	}
	// Anthropic 风格的鉴权文案
	if got := Classify(NewStatus(401, `{"error":{"type":"authentication_error","message":"invalid x-api-key"}}`, 0, nil)); got != KindAuth {
		t.Errorf("Anthropic 风格 401 应归为鉴权失败，实际 %v", got)
	}
}

// TestClassifyStatusKeepsContextOverflow 状态码判定**不能抢**超窗的优先级。
//
// 400 里混着「上下文超窗」，它不是格式错而是**可自救**的拒绝：
// agent 靠 KindContextOverflow 驱动「压缩历史后重发」这条命脉。
// 若先把 400 判成 KindParse，整条自救路断掉，而且测试不会红
// （现有 TestClassifyContextOverflow 用的是裸 errors.New，抓不到）。
func TestClassifyStatusKeepsContextOverflow(t *testing.T) {
	overflow := `{"error":{"code":"upstream_request_rejected","message":"This model's maximum context length is 262144 tokens. However, you requested 300000 tokens."}}`
	if got := Classify(NewStatus(400, overflow, 0, nil)); got != KindContextOverflow {
		t.Errorf("400 超窗应仍归为上下文超窗（压缩重发命脉），实际 %v", got)
	}
	// 反向守卫：400 里不是超窗的仍然是格式错
	if got := Classify(NewStatus(400, `{"error":{"message":"missing required parameter: model"}}`, 0, nil)); got != KindParse {
		t.Errorf("非超窗的 400 应归为格式错，实际 %v", got)
	}
}

// TestStatusErrorSurvivesWrapping 状态码要能穿透任意层 %w 包装。
//
// pkg/llm/sse.go 里有一处 `fmt.Errorf("%w（本次送出：%s）", apiErr, ...)`
// —— 用 %s 而不是 %w 的话链就断了，分类会静默失效。
func TestStatusErrorSurvivesWrapping(t *testing.T) {
	base := NewStatus(429, `{"code":"1305"}`, 0, nil)
	wrapped := fmt.Errorf("%w（本次送出：…）", base)

	if got := Classify(wrapped); got != KindRateLimit {
		t.Errorf("包装后应仍归为限流，实际 %v", got)
	}
	if got := StatusCode(wrapped); got != 429 {
		t.Errorf("StatusCode 应穿透包装，实际 %d", got)
	}
	// 断链的做法确实会丢状态码 —— 这条把「必须用 %w」钉成契约
	broken := fmt.Errorf("%s", base.Error())
	if got := StatusCode(broken); got != 0 {
		t.Errorf("用 %%s 断链后应取不到状态码，实际 %d（说明这条断言不再有意义）", got)
	}
	// 无状态码的错误
	if got := StatusCode(errors.New("普通错误")); got != 0 {
		t.Errorf("无状态码应返回 0，实际 %d", got)
	}
}

// TestAuthIsNeverRetryable 业务诉求直译：401 重试只是浪费配额并掩盖配置错误。
func TestAuthIsNeverRetryable(t *testing.T) {
	if Retryable(Classify(NewStatus(401, `{"error":{"code":"1113","message":"非法或过期的 API Key"}}`, 0, nil))) {
		t.Error("401 绝不能可重试")
	}
	if !Retryable(Classify(NewStatus(429, `{"code":"1305"}`, 0, nil))) {
		t.Error("429 退避重试才可能恢复，必须可重试")
	}
}

// TestStatusErrorMessageFormatKeepsParensCode 消息格式是**前端契约**。
//
// web/dist/ui.js 的 describeLLMError（:1467）与 retryReasonBrief（:1483）
// 都用 /\((\d{3})\)/ 从错误串里抠状态码，render_md.test.js 另有 12 条断言
// 覆盖 401/403/429/5xx 的中文提示。改这个格式会同时打破那 12 条。
func TestStatusErrorMessageFormatKeepsParensCode(t *testing.T) {
	for _, code := range []int{401, 403, 429, 503} {
		msg := NewStatus(code, "body", 0, nil).Error()
		if !strings.Contains(msg, fmt.Sprintf("(%d)", code)) {
			t.Errorf("状态码 %d 的消息必须保留 (%d) 形式（前端正则依赖），实际 %q", code, code, msg)
		}
	}
}

// TestStatusErrorCarriesRetryAfter Retry-After 必须随错误走上去。
//
// 否则上层只能说「稍后再试」，无法告诉用户「上游让你等 7 秒」。
func TestStatusErrorCarriesRetryAfter(t *testing.T) {
	se := NewStatus(429, "slow down", 7, nil)
	if se.RetryAfter != 7 {
		t.Errorf("RetryAfter 应为 7，实际 %d", se.RetryAfter)
	}
	if got := StatusRetryAfter(se); got != 7 {
		t.Errorf("StatusRetryAfter 应为 7，实际 %d", got)
	}
	if got := StatusRetryAfter(errors.New("普通错误")); got != 0 {
		t.Errorf("无 Retry-After 应为 0，实际 %d", got)
	}
}
