package agent

import (
	"context"
	"errors"
	"path/filepath"
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

// 部分内容后上游截断：保留已到内容，错误只 emit 一次，且不返回错误
// （本轮照常收尾，不能让截断的错误再被 WS 层重复下发）。
func TestRunLoopPartialErrorEmitsOnce(t *testing.T) {
	upstream := errors.New("上游截断")
	a := newEmitTestAgent(t, &partialProvider{err: upstream})
	sess := &Session{ID: "s-partial", Messages: []llm.Message{
		llm.TextMessage(llm.RoleUser, "写个网页"),
	}}
	counter := &countEmitter{}
	if err := a.runLoopWithPersistence(context.Background(), sess, counter.emit, false); err != nil {
		t.Fatalf("部分截断不应返回错误（内容保留收尾），实际: %v", err)
	}
	if n := counter.errors.Load(); n != 1 {
		t.Errorf("部分截断的错误应只 emit 一次，实际 %d 次", n)
	}
}
