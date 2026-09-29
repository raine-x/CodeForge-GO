package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"codeforge/pkg/errs"
	"codeforge/pkg/llm"
)

// 上游错误在**编排层**的表现。
//
// 这组测试守的是：401 一步到位上报（不压缩重发、不重复请求），
// 且用户看到的是「鉴权失败」而不是裸 JSON。
//
// 429 的退避重试已由 pkg/llm.postJSON 完成，编排层**不再叠一层** ——
// 叠了会放大成 3 × 5 = 15 次上游请求。

// TestAuthFailureIsNotRetriedOrShrunk 401 既不重试也不压缩。
func TestAuthFailureIsNotRetriedOrShrunk(t *testing.T) {
	var chatCalls int32
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if isSummaryRequest(req) {
			return okStream("摘要"), nil
		}
		atomic.AddInt32(&chatCalls, 1)
		return nil, errs.NewStatus(401, `{"error":{"code":"1113","message":"非法或过期的 API Key"}}`, 0, nil)
	}))
	sess := seedLongSession(t, a)

	err := a.Run(context.Background(), sess.ID, "继续", func(Event) {})
	if err == nil {
		t.Fatal("401 应报错")
	}
	if n := atomic.LoadInt32(&chatCalls); n != 1 {
		t.Errorf("401 不该重试，实际对话请求 %d 次", n)
	}
	// 用户看到的话术必须是「鉴权/密钥」而不是裸 JSON
	friendly := errs.FriendlyOr("生成回复", err)
	if !strings.Contains(friendly, "鉴权") && !strings.Contains(friendly, "密钥") {
		t.Errorf("401 应翻译成鉴权失败文案，实际 %q", friendly)
	}
	if strings.Contains(friendly, "1113") {
		t.Errorf("不该把上游原始 code 甩给用户: %q", friendly)
	}
}

// TestRateLimitIsRetriedOnlyAtTransport 429 的重试不在编排层。
//
// 编排层只认「上下文超窗」一种可自救的拒绝；429 已在 llm 层退避过了，
// 这里再叠一层就是 3 × 5 = 15 次请求。
func TestRateLimitIsRetriedOnlyAtTransport(t *testing.T) {
	var chatCalls int32
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if isSummaryRequest(req) {
			return okStream("摘要"), nil
		}
		atomic.AddInt32(&chatCalls, 1)
		return nil, errs.NewStatus(429, `{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}`, 0, nil)
	}))
	sess := seedLongSession(t, a)

	err := a.Run(context.Background(), sess.ID, "继续", func(Event) {})
	if err == nil {
		t.Fatal("429 应最终报错")
	}
	if n := atomic.LoadInt32(&chatCalls); n != 1 {
		t.Errorf("编排层不该给 429 叠重试（llm 层已退避过），实际对话请求 %d 次", n)
	}
	if got := errs.Classify(err); got != errs.KindRateLimit {
		t.Errorf("429 应归为限流，实际 %v", got)
	}
	// 上游的 code 不该被甩给用户
	friendly := errs.FriendlyOr("生成回复", err)
	if strings.Contains(friendly, "1305") {
		t.Errorf("不该把上游原始 code 甩给用户: %q", friendly)
	}
}
