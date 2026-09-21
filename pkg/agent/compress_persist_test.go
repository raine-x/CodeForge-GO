package agent

import (
	"context"
	"path/filepath"
	"strings"
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
