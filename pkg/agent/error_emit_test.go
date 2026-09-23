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

// failProvider 的 Stream 直接返回错误（复刻 402 余额不足这类上游失败）。
type failProvider struct{ err error }

func (p *failProvider) Name() string { return "fail-stub" }
func (p *failProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	return nil, p.err
}

// partialProvider 流到一半出错：已有部分文本 + EventError（上游截断）。
type partialProvider struct{ err error }

func (p *partialProvider) Name() string { return "partial-stub" }
func (p *partialProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "半句话"}
		ch <- llm.StreamEvent{Type: llm.EventError, Error: p.err.Error()}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// partialToolProvider 流出正文 + 一个工具调用（参数只来了一半）后报错。
// 参数被截断时 consumeStream 用 "{}" 兜底，关键是这条 tool_use 必须配到结果。
type partialToolProvider struct{ err error }

func (p *partialToolProvider) Name() string { return "partial-tool-stub" }
func (p *partialToolProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 6)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "我先读一下文件"}
		ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call_1", ToolName: "read_file"}
		ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call_1", InputDelta: `{"path":"a`}
		ch <- llm.StreamEvent{Type: llm.EventError, Error: p.err.Error()}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// 上游在工具调用生成中途报错时：正文和已拼装的 tool_use 都要入账，
// 且每条 tool_use 必须配到 tool_result —— 只有 tool_use 没有结果的序列
// 违反上游硬约束，下一轮请求会被严格上游直接 400（用户看到的是莫名其妙的
// 「请求格式错误」，而不是工具本身那句错误）。
func TestPartialTurnKeepsAssembledToolCalls(t *testing.T) {
	upstream := errors.New("上游连接中断")
	a := newEmitTestAgent(t, &partialToolProvider{err: upstream})
	sess := &Session{ID: "s-partial-tool", Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, "读一下 a.txt"),
	}}

	err := a.runLoopWithPersistence(context.Background(), sess, func(Event) {}, false)
	if err == nil || err.Error() != upstream.Error() {
		t.Fatalf("应把上游错误原样返回，实际: %v", err)
	}

	// 期望历史：user → assistant(正文 + tool_use) → tool_result(错误)。
	if len(sess.Messages) != 3 {
		t.Fatalf("半截回合应入账 3 条消息，实际 %d 条：%+v", len(sess.Messages), sess.Messages)
	}
	assistant := sess.Messages[1]
	if assistant.Role != llm.RoleAssistant {
		t.Fatalf("第 2 条应为助手消息，实际 %s", assistant.Role)
	}
	if assistant.Content[0].Type != llm.BlockText || assistant.Content[0].Text != "我先读一下文件" {
		t.Errorf("正文必须保留（界面已经显示过它），实际 %+v", assistant.Content[0])
	}
	use := assistant.Content[1]
	if use.Type != llm.BlockToolUse || use.ID != "call_1" || use.Name != "read_file" {
		t.Fatalf("已拼装的工具调用必须保留，实际 %+v", use)
	}
	if string(use.Input) != "{}" {
		t.Errorf("被截断的参数应兜底成 {}，实际 %s", use.Input)
	}
	res := sess.Messages[2].Content[0]
	if res.Type != llm.BlockToolResult || res.ToolUseID != "call_1" || !res.IsError {
		t.Fatalf("tool_use 必须配到错误结果，实际 %+v", res)
	}
	if !strings.Contains(res.Content, "未执行") {
		t.Errorf("结果应说明该调用从未执行，实际 %q", res.Content)
	}

	// 入账之后不得再有悬空 tool_use（修复函数无事可做）。
	if fixed := sess.repairDanglingToolUse(); len(fixed) != 0 {
		t.Errorf("入账后不应留下悬空 tool_use，实际补了 %v", fixed)
	}
}

// sigPartialProvider 流出两个工具调用：call_1 带思考签名，call_2 被截断、
// 签名还没到就报错 —— 模拟 Gemini 3 这类「签名是调用的一部分」的上游。
type sigPartialProvider struct{ err error }

func (p *sigPartialProvider) Name() string { return "sig-partial-stub" }
func (p *sigPartialProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call_1", ToolName: "read_file"}
		ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call_1", ThoughtSig: "SIG-1"}
		ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call_1", InputDelta: `{"path":"a.txt"}`}
		ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call_2", ToolName: "read_file"}
		ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call_2", InputDelta: `{"path":"b`}
		ch <- llm.StreamEvent{Type: llm.EventError, Error: p.err.Error()}
	}()
	return ch, nil
}

// 上游用思考签名时，缺签名的 tool_use 绝不能补进历史：回送会被上游以
// 「Function call is missing a thought_signature」直接 400 ——
// 那是比「丢掉一条没跑成的调用」严重得多的后果。
func TestPartialTurnDropsUnsignedToolCall(t *testing.T) {
	upstream := errors.New("上游连接中断")
	a := newEmitTestAgent(t, &sigPartialProvider{err: upstream})
	sess := &Session{ID: "s-sig", Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, "读两个文件"),
	}}

	if err := a.runLoopWithPersistence(context.Background(), sess, func(Event) {}, false); err == nil {
		t.Fatal("上游报错应向上返回")
	}

	var uses, results []string
	for _, m := range sess.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				uses = append(uses, b.ID)
				if b.ThoughtSig == "" {
					t.Errorf("补进历史的 tool_use 必须带签名，实际 %+v", b)
				}
			case llm.BlockToolResult:
				results = append(results, b.ToolUseID)
			}
		}
	}
	if len(uses) != 1 || uses[0] != "call_1" {
		t.Fatalf("只应保留带签名的 call_1，实际 %v", uses)
	}
	if len(results) != 1 || results[0] != "call_1" {
		t.Fatalf("保留的 tool_use 必须配到结果，实际 %v", results)
	}
	// 序列仍然合法：没有悬空 tool_use。
	if fixed := sess.repairDanglingToolUse(); len(fixed) != 0 {
		t.Errorf("入账后不应留下悬空 tool_use，实际补了 %v", fixed)
	}
}

// newEmitTestAgent 构造一个只接了空注册表与临时存储的 Agent，跑 runLoop 够用
// （systemPromptFor 会读 History 的任务清单，history 为 nil 会 panic）。
func newEmitTestAgent(t *testing.T, p llm.Provider) *Agent {
	t.Helper()
	registry := tools.NewRegistry()
	executor := tools.NewExecutor(registry, security.NewPolicy(config.SecurityConfig{}), nil, nil, 0, 0)
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(config.AgentConfig{MaxSteps: 3}, config.LLMConfig{}, p, executor, NewHistory(st), "")
}

// countEmitter 统计各类事件（重点数 EventError 出现次数）。
type countEmitter struct{ errors atomic.Int32 }

func (c *countEmitter) emit(ev Event) {
	if ev.Type == EventError {
		c.errors.Add(1)
	}
}

// 致命错误（Stream 直接失败）：循环自身不得 emit，错误原样返回给调用方
// （WS 层 ws_handler.run 统一 emit 一次）。2026-09-16 的双报错 bug 就是
// 循环里 emit 一次 + WS 层再 emit 一次，界面上同一条 402 显示两遍。
func TestRunLoopFatalErrorEmitsOnce(t *testing.T) {
	upstream := errors.New("LLM 请求失败 (402): billing_error")
	a := newEmitTestAgent(t, &failProvider{err: upstream})
	sess := &Session{ID: "s-fatal", Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, "写个网页"),
	}}
	counter := &countEmitter{}
	err := a.runLoopWithPersistence(context.Background(), sess, counter.emit, false)
	if err == nil || !errors.Is(err, upstream) {
		t.Fatalf("应把上游错误返回给调用方，实际: %v", err)
	}
	if n := counter.errors.Load(); n != 0 {
		t.Errorf("致命错误不应由循环 emit（调用方统一发一次），实际 emit %d 次", n)
	}
}

func TestRunLoopPartialErrorEmitsOnce(t *testing.T) {
	upstream := errors.New("上游截断")
	a := newEmitTestAgent(t, &partialProvider{err: upstream})
	sess := &Session{ID: "s-partial", Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, "写个网页"),
	}}
	counter := &countEmitter{}
	var done bool
	err := a.runLoopWithPersistence(context.Background(), sess, func(ev Event) {
		counter.emit(ev)
		done = done || ev.Type == EventDone
	}, false)
	if err == nil || err.Error() != upstream.Error() {
		t.Fatalf("expected upstream error, got %v", err)
	}
	if n := counter.errors.Load(); n != 0 || done {
		t.Fatalf("error must be returned without success or duplicate emission: errors=%d done=%v", n, done)
	}
	if len(sess.Messages) != 2 || sess.Messages[1].Content[0].Text != "半句话" {
		t.Fatalf("partial text was not preserved: %v", sess.Messages)
	}
}
