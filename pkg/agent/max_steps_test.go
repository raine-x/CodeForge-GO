package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/llm"
)

type maxStepsProvider func(context.Context, llm.Request) (<-chan llm.StreamEvent, error)

func (maxStepsProvider) Name() string { return "max-steps-stub" }

func (p maxStepsProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	return p(ctx, req)
}

func maxStepsToolStream() <-chan llm.StreamEvent {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "working"}
	ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call", ToolName: "missing_test_tool"}
	ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call", InputDelta: "{}"}
	close(ch)
	return ch
}

func TestMaxStepsSnapshotAndHistory(t *testing.T) {
	calls := 0
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		calls++
		return maxStepsToolStream(), nil
	}))
	sess, err := a.History().Create("", "limit")
	if err != nil {
		t.Fatal(err)
	}
	updated := make(chan struct{})
	update := make(chan struct{})
	go func() {
		<-update
		a.SetMaxSteps(1)
		close(updated)
	}()
	steps, done := 0, 0
	emit := func(ev Event) {
		if ev.Type == EventUser {
			close(update)
			<-updated
		}
		if ev.Type == EventStep {
			steps++
		}
		if ev.Type == EventDone {
			done++
		}
	}
	err = a.Run(context.Background(), sess.ID, "continue", emit)
	if err == nil || !strings.Contains(err.Error(), "3 轮") || !strings.Contains(err.Error(), "设置 > 常规") {
		t.Fatalf("expected limit and settings hint, got %v", err)
	}
	if calls != 3 || steps != 3 || done != 0 || a.MaxSteps() != 1 {
		t.Fatalf("calls=%d steps=%d done=%d max=%d", calls, steps, done, a.MaxSteps())
	}
	loaded, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok || len(loaded.Messages) != 7 || !reflect.DeepEqual(loaded.Messages, sess.Messages) {
		t.Fatal("full history was not persisted")
	}
	calls = 0
	err = a.Regenerate(context.Background(), sess.ID, func(ev Event) {
		if ev.Type == EventDone {
			t.Error("limit must not emit done")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "1 轮") || calls != 1 || len(sess.Messages) != 3 {
		t.Fatalf("regenerate did not use updated limit: calls=%d err=%v", calls, err)
	}
	for _, limit := range []int{100, 1} {
		a.SetMaxSteps(limit)
		calls = 0
		err = a.Run(context.Background(), sess.ID, "next", func(ev Event) {
			if ev.Type == EventDone {
				t.Error("limit must not emit done")
			}
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d 轮", limit)) || calls != limit {
			t.Fatalf("limit=%d calls=%d err=%v", limit, calls, err)
		}
	}
}

// TestMaxStepsSubagentInheritsParent 锁住「子智能体步数默认跟随主 loop」。
//
// 早先这里写死 8：探索类任务经常要十几步，8 步会在半途硬停，模型被迫交一份
// 「还没看完」的结论，主智能体再接着做等于把活儿又干一遍。现在默认继承，
// 显式配置（subagents.max_steps）才覆盖。
func TestMaxStepsSubagentInheritsParent(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	for _, tc := range []struct{ parent, child int }{{1, 1}, {8, 8}, {12, 12}, {0, 8}} {
		a.SetMaxSteps(tc.parent)
		child := a.newSubagent("explore")
		steps := 0
		err := child.runLoopEphemeral(context.Background(), &Session{ID: "child"}, func(ev Event) {
			if ev.Type == EventStep {
				steps++
			}
			if ev.Type == EventDone {
				t.Error("exhausted child must not emit done")
			}
		})
		if child.MaxSteps() != tc.child || steps != tc.child || err == nil {
			t.Fatalf("parent=%d child=%d steps=%d err=%v", tc.parent, child.MaxSteps(), steps, err)
		}
	}
}

// TestMaxStepsSubagentPolicyOverride 显式配置优先，且越界值按硬上限收敛。
func TestMaxStepsSubagentPolicyOverride(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	for _, tc := range []struct{ parent, want, policy int }{
		{25, 4, 4},                        // 显式调小
		{25, 25, 0},                       // 未配置 = 继承
		{25, 25, -1},                      // 负数同样按「继承」处理
		{25, config.SubagentStepCap, 999}, // 越界收敛到硬上限，不拒绝
	} {
		a.SetMaxSteps(tc.parent)
		a.SetSubagentPolicy(SubagentPolicy{MaxConcurrent: 1, MaxSteps: tc.policy}.Normalize())
		if got := a.newSubagent("explore").MaxSteps(); got != tc.want {
			t.Errorf("parent=%d policy=%d 期望 %d，实际 %d", tc.parent, tc.policy, tc.want, got)
		}
	}
}

// TestSubagentInheritsContextWindow 锁住上下文窗口的继承。
//
// contextWindow 是 Agent 上的 atomic，New() 不填；不显式继承的话子智能体
// 拿到的窗口是 0 → 压缩线回退到写死的 context_token_budget（120000）。
// 模型真实窗口若小于该值，子智能体就会堆到上游拒绝才压缩 → 子任务直接失败。
func TestSubagentInheritsContextWindow(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	a.SetContextWindow(32768)
	if got := a.newSubagent("explore").ContextWindow(); got != 32768 {
		t.Fatalf("子智能体上下文窗口期望继承 32768，实际 %d", got)
	}
	// 主智能体窗口未知时也不该给子智能体编一个出来。
	a.SetContextWindow(0)
	if got := a.newSubagent("explore").ContextWindow(); got != 0 {
		t.Fatalf("窗口未知时期望 0，实际 %d", got)
	}
}

// TestSubagentDropsBuiltinPlugins 锁住「内置插件开关不继承」。
//
// 子智能体白名单里没有 create_skill / delegate_subagents，继承了开关就会让
// System Prompt 注入「你可以创建技能 / 委派子智能体」，模型反复调用不存在的
// 工具直到步数耗尽。
func TestSubagentDropsBuiltinPlugins(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	a.SetSkillCreatorEnabled(true)
	a.SetMultiAgentEnabled(true)
	a.SetPlanEnabled(true)
	child := a.newSubagent("explore")
	if got := child.builtinOnSnapshot(); len(got) != 0 {
		t.Fatalf("子智能体不该继承内置插件开关，实际 %v", got)
	}
	// 基础提示词里「委派给子智能体」是通用工作流纪律，保留；
	// 要断言的是**内置插件注入段**整段消失（它承诺的工具子智能体根本没有）。
	if s := child.builtinPluginSection(); strings.TrimSpace(s) != "" {
		t.Fatalf("子智能体不该有内置插件注入段，实际 %q", s)
	}
	if s := child.systemPromptFor(&Session{ID: "c"}); strings.Contains(s, "## 内置插件：") {
		t.Fatal("子智能体的系统提示词里不该出现内置插件注入段")
	}
}

func TestMaxStepsCancellation(t *testing.T) {
	for _, stage := range []string{"before", "stream", "last_tool"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
				calls++
				if stage == "stream" {
					cancel()
					return make(chan llm.StreamEvent), nil
				}
				return maxStepsToolStream(), nil
			}))
			a.SetMaxSteps(1)
			if stage == "before" {
				cancel()
			}
			finished := make(chan error, 1)
			done := false
			go func() {
				finished <- a.runLoopEphemeral(ctx, &Session{ID: "cancel"}, func(ev Event) {
					if ev.Type == EventToolResult && stage == "last_tool" {
						cancel()
					}
					done = done || ev.Type == EventDone
				})
			}()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) || done || (stage == "before" && calls != 0) {
					t.Fatalf("calls=%d done=%v err=%v", calls, done, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation blocked")
			}
		})
	}
}

func TestMaxStepsNormalCompletion(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "complete"}
		close(ch)
		return ch, nil
	}))
	a.SetMaxSteps(1)
	done := 0
	err := a.runLoopEphemeral(context.Background(), &Session{ID: "complete"}, func(ev Event) {
		if ev.Type == EventDone {
			done++
		}
	})
	if err != nil || done != 1 {
		t.Fatalf("done=%d err=%v", done, err)
	}
}
