package agent

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"

	"codeforge/pkg/llm"
)

// ---------- 估算器校准 ----------

// 上游回报的真实输入比估算大 → 系数应大于 1（后续按比例放大，压缩才会按时触发）。
func TestCalibrateTokenFactorAmplifiesOnUnderestimate(t *testing.T) {
	s := &Session{reqEstimate: 1000}
	s.calibrateTokenFactor(1500) // 真实比估算多 50%
	if s.tokenFactor != 1.5 {
		t.Fatalf("系数应为 1.5，实际 %v", s.tokenFactor)
	}
}

// 真实值比估算**小**时不能把系数降到 1 以下 —— 宁可早压，不能漏压。
func TestCalibrateTokenFactorNeverShrinks(t *testing.T) {
	s := &Session{reqEstimate: 1000}
	s.calibrateTokenFactor(600)
	if s.tokenFactor != 1 {
		t.Fatalf("系数下限应为 1（只放大不缩小），实际 %v", s.tokenFactor)
	}
}

// 异常用量不能把系数顶到离谱高度（否则压缩线被压得过低、每轮都在无谓压缩）。
func TestCalibrateTokenFactorIsCapped(t *testing.T) {
	s := &Session{reqEstimate: 100}
	s.calibrateTokenFactor(100000)
	if s.tokenFactor != maxTokenFactor {
		t.Fatalf("系数应被封顶到 %v，实际 %v", maxTokenFactor, s.tokenFactor)
	}
}

// 没有估算基准 / 没有真实值时不该乱改系数。
func TestCalibrateTokenFactorIgnoresMissingData(t *testing.T) {
	s := &Session{}
	s.calibrateTokenFactor(12345)
	if s.tokenFactor != 0 {
		t.Errorf("无估算基准时不该校准，实际 %v", s.tokenFactor)
	}
	s2 := &Session{reqEstimate: 1000}
	s2.calibrateTokenFactor(0)
	if s2.tokenFactor != 0 {
		t.Errorf("无真实用量时不该校准，实际 %v", s2.tokenFactor)
	}
}

// 校准后的估算 = 裸估算 × 系数。
//
// ⚠️ **未校准**时用的是保守系数（uncalibratedTokenFactor），不是 1.0 ——
// 估算口径对代码/JSON 稳定低估约 10%，用 1.0 会让重启后的第一次请求
// 带着超窗的体量发出去（「每次启动的第一次老是炸上下文」，2026-09-21 反馈）。
func TestCalibratedEstimate(t *testing.T) {
	msgs := []llm.Message{llm.TextMessage(llm.RoleUser, strings.Repeat("a", 400))}
	base := EstimateTokens(msgs)

	// 未校准：应放大到保守系数（实现用四舍五入，不是截断）
	raw := &Session{}
	wantUncal := int(math.Round(float64(base) * uncalibratedTokenFactor))
	if got := raw.calibratedEstimate(msgs); got != wantUncal {
		t.Errorf("未校准时应按保守系数 %v 放大为 %d，实际 %d",
			uncalibratedTokenFactor, wantUncal, got)
	}
	if got := raw.calibratedEstimate(msgs); got <= base {
		t.Error("未校准时也不该退回裸估算（那正是「第一次炸上下文」的成因）")
	}

	// 已校准：用真实比值
	cal := &Session{tokenFactor: 1.5}
	want := int(float64(base) * 1.5)
	if got := cal.calibratedEstimate(msgs); got != want {
		t.Errorf("校准后应为 %d，实际 %d", want, got)
	}

	// 空消息不该算出 0 或负数
	if got := raw.calibratedEstimate(nil); got != 0 {
		t.Errorf("空消息应为 0，实际 %d", got)
	}
}

// ---------- 超窗 → 压缩重试 ----------

// 实测那条上游报错原文（2026-09-21）。
const overflowErrText = `LLM 请求失败 (400): {"error":{"code":"upstream_request_rejected",` +
	`"message":"This model's maximum context length is 262144 tokens. However, ` +
	`you requested 128000 output tokens and your prompt contains at least 134145 ` +
	`input tokens, for a total of at least 262145 tokens. Please reduce the length ` +
	`of the input prompt or the number of requested output tokens.",` +
	`"type":"invalid_request_error"}}`

// isSummaryRequest 判断一次 provider 调用是不是「压缩用的摘要请求」。
//
// ⚠️ 必须区分开：压缩时会额外调一次 provider 做摘要，把它算进重试次数会得出
// 错误的结论（第一版就因此算出「应 3 次实际 6 次」）。
// 用 system prompt 前缀判别 —— 摘要请求用的是固定的 summarySystemPrompt。
func isSummaryRequest(req llm.Request) bool {
	return strings.HasPrefix(req.System, "你是 CodeForge 的上下文压缩器")
}

// okStream 返回一条正常的收尾流。
func okStream(text string) <-chan llm.StreamEvent {
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: text}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch
}

// seedLongSession 造出一段足够长的历史，让压缩真的会触发。
//
// 窗口取 20000 是因为测试环境的系统提示词本身就占 3568 token：
// 窗口小于它会让 budget 算成负数，走的是「提示词占满预算」那条早退分支，
// 根本测不到超窗重试。
func seedLongSession(t *testing.T, a *Agent) *Session {
	t.Helper()
	a.SetContextWindow(20000)
	sess, err := a.History().Create("", "overflow")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("上下文填充内容。", 800)),
			llm.TextMessage(llm.RoleAssistant, strings.Repeat("回答填充内容。", 800)),
		)
	}
	a.save(sess, true)
	return sess
}

// 上游以「上下文超窗」拒绝时，必须**压得更狠再试**，而不是直接失败。
//
// 这是 2026-09-21 实测故障的回归测试：输入 134145 + 输出预留 128000 超过
// 262144 窗口，上游回 400 upstream_request_rejected，整轮任务直接白跑。
func TestContextOverflowCompressesAndRetries(t *testing.T) {
	var chatCalls int32
	var firstTokens, secondTokens int

	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if isSummaryRequest(req) {
			return okStream("摘要"), nil
		}
		switch atomic.AddInt32(&chatCalls, 1) {
		case 1:
			firstTokens = EstimateTokens(req.Messages)
			return nil, errors.New(overflowErrText)
		default:
			secondTokens = EstimateTokens(req.Messages)
			return okStream("好的"), nil
		}
	}))
	sess := seedLongSession(t, a)

	var gotError string
	err := a.Run(context.Background(), sess.ID, "继续", func(ev Event) {
		if ev.Type == EventError {
			gotError = ev.Error
		}
	})

	if err != nil {
		t.Fatalf("超窗后应压缩重试，而不是直接失败: %v", err)
	}
	if gotError != "" {
		t.Fatalf("自救成功时不该报错给用户，实际: %s", gotError)
	}
	if n := atomic.LoadInt32(&chatCalls); n != 2 {
		t.Fatalf("对话请求应重试一次（共 2 次），实际 %d 次", n)
	}
	if secondTokens <= 0 || firstTokens <= 0 {
		t.Fatalf("没抓到两次请求的体量：first=%d second=%d", firstTokens, secondTokens)
	}
	// ⚠️ 这里只断言「不会变大」，而不是「一定变小」。
	// 因为本用例的历史远超预算，**第一次就已经压到极限**，收紧预算也没有
	// 进一步空间 —— 两次体量相同是正常的。真正要守住的是「重试绝不让载荷变大」
	//（变大意味着压缩线收紧没生效，重发必然再次超窗）。
	if secondTokens > firstTokens {
		t.Errorf("重试的载荷不该变大：第一次 %d tokens → 第二次 %d tokens",
			firstTokens, secondTokens)
	}
}

// 收紧次数用尽后仍超窗 → 如实报错，不无限重试。
func TestContextOverflowGivesUpAfterMaxShrinks(t *testing.T) {
	var chatCalls int32
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if isSummaryRequest(req) {
			return okStream("摘要"), nil
		}
		atomic.AddInt32(&chatCalls, 1)
		return nil, errors.New(overflowErrText)
	}))
	sess := seedLongSession(t, a)

	if err := a.Run(context.Background(), sess.ID, "继续", func(Event) {}); err == nil {
		t.Fatal("持续超窗最终应报错")
	}
	// 首次 + maxOverflowShrinks 次收紧重试
	if want := int32(1 + maxOverflowShrinks); atomic.LoadInt32(&chatCalls) != want {
		t.Errorf("对话请求应尝试 %d 次（1 次首发 + %d 次收紧重试），实际 %d 次",
			want, maxOverflowShrinks, atomic.LoadInt32(&chatCalls))
	}
}

// 非超窗错误（如鉴权失败）不该走「压缩重试」这条路 —— 压多少次也没用。
func TestNonOverflowErrorIsNotRetriedByShrinking(t *testing.T) {
	var chatCalls int32
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if isSummaryRequest(req) {
			return okStream("摘要"), nil
		}
		atomic.AddInt32(&chatCalls, 1)
		return nil, errors.New(`LLM 请求失败 (401): {"error":{"message":"invalid api key"}}`)
	}))
	sess := seedLongSession(t, a)

	if err := a.Run(context.Background(), sess.ID, "继续", func(Event) {}); err == nil {
		t.Fatal("鉴权失败应报错")
	}
	if n := atomic.LoadInt32(&chatCalls); n != 1 {
		t.Errorf("鉴权失败不该重试，实际对话请求 %d 次", n)
	}
}
