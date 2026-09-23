package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// newEmitTestAgentWithStore 用**已有的** store 构造 Agent —— 用于模拟重启：
// 同一份数据库、全新的 Agent 实例（内存缓存为空）。
//
// provider 传 nil 即可：这些用例只走 History() / requestView()，不会发起请求。
func newEmitTestAgentWithStore(t *testing.T, st *store.Store) *Agent {
	t.Helper()
	registry := tools.NewRegistry()
	executor := tools.NewExecutor(registry, security.NewPolicy(config.SecurityConfig{}), nil, nil, 0, 0)
	return New(config.AgentConfig{MaxSteps: 3}, config.LLMConfig{}, nil, executor, NewHistory(st), "")
}

// 压缩状态必须**跨重启保留**。
//
// 2026-09-21 实测反馈：「重新打开同一个对话（重启服务）后，已经压缩的上下文
// 似乎并没有保留上一次被压缩的状态，因为我重新打开后直接显示超出上下文限制了」。
//
// 根因：compressedUpTo / summaryText 此前只存在内存里，从不落盘 ——
// 重启后被摘要覆盖的那段历史又原样送了上去，刚压到线下的会话立刻又超窗。
func TestCompressionStateSurvivesRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	h1 := NewHistory(st)
	sess, err := h1.Create("", "restart")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("历史内容。", 20)),
			llm.TextMessage(llm.RoleAssistant, strings.Repeat("回复内容。", 20)),
		)
	}
	// 模拟「已经压缩过」：前 8 条被摘要覆盖
	sess.compressedUpTo = 8
	sess.summaryText = "这是被压缩掉的 8 条历史的摘要"
	if err := h1.Save(sess.ID); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：同一份数据库，全新的 History（缓存为空）
	h2 := NewHistory(st)
	got, ok := h2.Get(sess.ID)
	if !ok {
		t.Fatal("重启后应能读回会话")
	}
	if got.compressedUpTo != 8 {
		t.Errorf("压缩进度应保留 8，实际 %d", got.compressedUpTo)
	}
	if got.summaryText != "这是被压缩掉的 8 条历史的摘要" {
		t.Errorf("摘要应保留，实际 %q", got.summaryText)
	}
}

// 压缩后的「送模视图」在重启后必须仍然更小 —— 这才是用户真正感知到的东西。
func TestCompressedViewStaysSmallAfterRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	a1 := newEmitTestAgentWithStore(t, st)
	sess, err := a1.History().Create("", "restart-view")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("很长的历史内容。", 100)),
			llm.TextMessage(llm.RoleAssistant, strings.Repeat("很长的回复内容。", 100)),
		)
	}
	sess.compressedUpTo = 12
	sess.summaryText = "摘要：前面聊了很多内容。"
	if err := a1.History().Save(sess.ID); err != nil {
		t.Fatal(err)
	}
	full := EstimateTokens(sess.Messages)

	// 重启
	a2 := newEmitTestAgentWithStore(t, st)
	got, ok := a2.History().Get(sess.ID)
	if !ok {
		t.Fatal("重启后应能读回会话")
	}
	view := a2.requestView(got)
	compressed := EstimateTokens(view)

	if compressed >= full {
		t.Errorf("重启后送模视图应仍被压缩：完整 %d → 压缩后 %d", full, compressed)
	}
	if compressed == 0 {
		t.Error("压缩后不该是空的")
	}
}

// 压缩状态越界时必须自愈（历史可能被回退或整体替换过）。
func TestCompressionStateOutOfRangeIsHealed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	h1 := NewHistory(st)
	sess, err := h1.Create("", "stale")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "只有一条"))
	// 故意写一个越界的压缩进度（比如历史被回退后残留）
	sess.compressedUpTo = 99
	sess.summaryText = "陈旧摘要"
	if err := h1.Save(sess.ID); err != nil {
		t.Fatal(err)
	}

	h2 := NewHistory(st)
	got, ok := h2.Get(sess.ID)
	if !ok {
		t.Fatal("应能读回会话")
	}
	if got.compressedUpTo != 0 || got.summaryText != "" {
		t.Errorf("越界的压缩状态应被复位，实际 upTo=%d summary=%q",
			got.compressedUpTo, got.summaryText)
	}
}

// ---------- 切换模型前的超窗判定 ----------

// 判定必须用**压缩后的实际送模量**，不是原始历史。
//
// 这是 2026-09-21 反馈的核心：压缩过的会话原始历史仍然很大，
// 用原始量判会把「已经压好、本来完全跑得动」的会话也判成超窗，
// 服务端据此拦住切换 —— 用户就会遇到「明明刚压缩过，一切换又说超限」。
func TestSessionOverflowForUsesCompressedView(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
		return nil, nil
	}))
	sess, err := a.History().Create("", "overflow-check")
	if err != nil {
		t.Fatal(err)
	}
	// 造一段很大的原始历史
	for i := 0; i < 10; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("很长的历史内容。", 200)),
			llm.TextMessage(llm.RoleAssistant, strings.Repeat("很长的回复内容。", 200)),
		)
	}
	raw := EstimateTokens(sess.Messages)

	// 目标窗口比原始历史小 → 未压缩时确实判超窗
	small := raw / 2
	if got := a.SessionOverflowFor(sess, small); got <= 0 {
		t.Fatalf("未压缩时应判超窗（raw=%d 窗口=%d），实际 %d", raw, small, got)
	}

	// 压缩掉大部分历史后，同样的窗口应当放得下
	sess.compressedUpTo = len(sess.Messages) - 2
	sess.summaryText = "摘要：前面聊了很多内容。"
	if got := a.SessionOverflowFor(sess, small); got != 0 {
		t.Errorf("压缩后应放得下（窗口 %d），实际判超出 %d tokens", small, got)
	}
}

// 窗口未知（<=0）时不判超窗 —— 没有依据就别拦。
func TestSessionOverflowForUnknownWindow(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
		return nil, nil
	}))
	sess, _ := a.History().Create("", "unknown-window")
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, strings.Repeat("内容。", 500)))

	if got := a.SessionOverflowFor(sess, 0); got != 0 {
		t.Errorf("窗口未知时不该判超窗，实际 %d", got)
	}
	if got := a.SessionOverflowFor(nil, 1000); got != 0 {
		t.Errorf("会话为空时不该判超窗，实际 %d", got)
	}
}

// RequestViewTokens 返回的是压缩后的送模量，不是原始历史。
func TestRequestViewTokensIsCompressedSize(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
		return nil, nil
	}))
	sess, _ := a.History().Create("", "view-tokens")
	for i := 0; i < 10; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("很长的历史内容。", 200)),
		)
	}
	raw := EstimateTokens(sess.Messages)
	sess.compressedUpTo = len(sess.Messages) - 1
	sess.summaryText = "摘要"

	view := a.RequestViewTokens(sess)
	if view >= raw {
		t.Errorf("送模量应小于原始历史：raw=%d view=%d", raw, view)
	}
	if view <= 0 {
		t.Error("送模量不该为 0")
	}
}

// ---------- 摘要单块失败 → 减半重试，而不是回退机械压缩 ----------

// 2026-09-21 反馈：界面上出现「上下文超出阈值，已临时压缩历史（210.7k → 52.8k
// tokens）。摘要不可用，已回退机械压缩」—— 机械压缩是**直接丢弃**历史，
// 用户的原话是「不要直接截断，可以分块压缩啊」。
//
// 本用例模拟「块太大导致摘要失败」：第一次大块必失败，减半后应成功。
func TestSummarizeChunkFailureShrinksInsteadOfDegrading(t *testing.T) {
	var summaryCalls int32
	var firstSize, laterSize int

	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if !isSummaryRequest(req) {
			// 对话请求：正常收尾
			return okStream("好的"), nil
		}
		size := EstimateTokens(req.Messages)
		n := atomic.AddInt32(&summaryCalls, 1)
		if n == 1 {
			firstSize = size
		}
		// 模拟「块太大 → 摘要超时」：超过阈值就失败
		const failAbove = 4000
		if size > failAbove {
			return nil, errors.New("摘要超时或中断: context deadline exceeded")
		}
		laterSize = size
		return okStream("这是摘要内容。"), nil
	}))
	sess := seedLongSession(t, a)

	var degraded bool
	err := a.Run(context.Background(), sess.ID, "继续", func(ev Event) {
		if ev.Type == EventCompress && ev.Compress != nil && ev.Compress.Degraded {
			degraded = true
		}
	})

	if degraded {
		t.Errorf("单块失败后应减半重试，不该回退机械压缩（= 丢弃历史）；摘要调用 %d 次，首次 %d tokens，末次 %d tokens",
			atomic.LoadInt32(&summaryCalls), firstSize, laterSize)
	}
	if err != nil {
		t.Fatalf("压缩重试后本轮应能正常完成: %v", err)
	}
	if n := atomic.LoadInt32(&summaryCalls); n < 2 {
		t.Errorf("应至少重试一次（大块失败 → 减半重来），实际摘要请求 %d 次", n)
	}
	if laterSize >= firstSize {
		t.Errorf("重试时块应变小：第一次 %d tokens → 后来 %d tokens", firstSize, laterSize)
	}
}

// 摘要彻底不可用（一直失败）时，仍要回退机械压缩 —— 这是最后的保命手段，
// 不能因为「宁可丢历史也不报错」把整轮卡死。
func TestSummarizePersistentFailureStillDegrades(t *testing.T) {
	var summaryCalls int32
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		if !isSummaryRequest(req) {
			return okStream("好的"), nil
		}
		atomic.AddInt32(&summaryCalls, 1)
		return nil, errors.New("上游持续故障")
	}))
	sess := seedLongSession(t, a)

	err := a.Run(context.Background(), sess.ID, "继续", func(Event) {})
	if err != nil {
		t.Fatalf("摘要持续失败时应回退机械压缩并继续，而不是整轮失败: %v", err)
	}
	// 减半重试到下限为止
	if n := atomic.LoadInt32(&summaryCalls); n < 2 {
		t.Errorf("应重试到下限，实际摘要请求 %d 次", n)
	}
}

// 估算器校准系数必须跨重启保留 —— 这是「每次启动第一次不炸上下文」的关键。
//
// 2026-09-21 反馈：「每次启动的第一次老是炸上下文」。
// 成因：tokenFactor 不落盘 → 重启归零 → 第一次请求用最乐观的估算判定
// （代码/JSON 实际约 3–3.5 字符/token，估算按 4 计，稳定低估约 10%），
// 判定「没超预算」就把完整历史发出去，被上游 400 拒绝。
func TestTokenFactorSurvivesRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	h1 := NewHistory(st)
	sess, err := h1.Create("", "factor")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "内容"))
	sess.tokenFactor = 1.42 // 实测校准出来的系数
	if err := h1.Save(sess.ID); err != nil {
		t.Fatal(err)
	}

	// 模拟重启
	h2 := NewHistory(st)
	got, ok := h2.Get(sess.ID)
	if !ok {
		t.Fatal("重启后应能读回会话")
	}
	if got.tokenFactor != 1.42 {
		t.Errorf("校准系数应保留 1.42，实际 %v（归零会导致第一次请求炸上下文）", got.tokenFactor)
	}
}

// 未校准过的会话读回来应当是 0（= 不放大），不能变成 NaN 之类。
func TestTokenFactorDefaultIsZero(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	h := NewHistory(st)
	sess, _ := h.Create("", "fresh")
	if err := h.Save(sess.ID); err != nil {
		t.Fatal(err)
	}
	h2 := NewHistory(st)
	got, _ := h2.Get(sess.ID)
	if got.tokenFactor != 0 {
		t.Errorf("未校准应为 0，实际 %v", got.tokenFactor)
	}
	if got.calibratedEstimate([]llm.Message{llm.TextMessage(llm.RoleUser, "abc")}) <= 0 {
		t.Error("系数为 0 时估算不该变成 0 或负数")
	}
}

// 校准系数落盘后，重启回来的第一次判定就该是「保守」的。
func TestCalibrationAffectsEstimateAfterRestart(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	a1 := newEmitTestAgentWithStore(t, st)
	sess, _ := a1.History().Create("", "calib")
	for i := 0; i < 20; i++ {
		sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, strings.Repeat("内容。", 50)))
	}
	sess.tokenFactor = 1.3
	if err := a1.History().Save(sess.ID); err != nil {
		t.Fatal(err)
	}

	a2 := newEmitTestAgentWithStore(t, st)
	got, _ := a2.History().Get(sess.ID)
	raw := EstimateTokens(got.Messages)
	calibrated := got.calibratedEstimate(got.Messages)
	if calibrated <= raw {
		t.Errorf("重启后估算应按系数放大：raw=%d calibrated=%d", raw, calibrated)
	}
}
