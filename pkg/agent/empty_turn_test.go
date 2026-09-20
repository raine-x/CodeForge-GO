package agent

import (
	"context"
	"strings"
	"testing"

	"codeforge/pkg/llm"
)

// 空回合回归：上游只回思考、或流被静默截断时，本轮既没有正文也没有工具调用。
// 旧实现把这种回合当成「模型答完了」正常收摊，界面上就是任务凭空停了、
// 一句话也没有（实测 glm-4.7-flash 连读十余个文件后出现过一次）。

func streamOf(events ...llm.StreamEvent) <-chan llm.StreamEvent {
	ch := make(chan llm.StreamEvent, len(events)+1)
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// scriptProvider 按脚本逐轮返回事件流；脚本用尽后重复最后一项。
type scriptProvider struct {
	scripts []func() <-chan llm.StreamEvent
	calls   int
}

func (p *scriptProvider) Name() string { return "script-stub" }

func (p *scriptProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	i := p.calls
	if i >= len(p.scripts) {
		i = len(p.scripts) - 1
	}
	p.calls++
	return p.scripts[i](), nil
}

func stopped() llm.StreamEvent { return llm.StreamEvent{Type: llm.EventMessageStop} }

func TestEmptyTurnRetriesOnceThenCompletes(t *testing.T) {
	p := &scriptProvider{scripts: []func() <-chan llm.StreamEvent{
		func() <-chan llm.StreamEvent {
			return streamOf(
				llm.StreamEvent{Type: llm.EventReasoningDelta, Text: "让我把这些文件汇总一下……"},
				stopped())
		},
		func() <-chan llm.StreamEvent {
			return streamOf(llm.StreamEvent{Type: llm.EventTextDelta, Text: "这是完整的分析结论"})
		},
	}}
	a := newEmitTestAgent(t, p)
	sess, err := a.History().Create("", "空回合")
	if err != nil {
		t.Fatal(err)
	}
	var retried, done int
	var reply strings.Builder
	emit := func(ev Event) {
		switch ev.Type {
		case EventRetry:
			retried++
		case EventDone:
			done++
		case EventText:
			reply.WriteString(ev.Text)
		}
	}
	if err := a.Run(context.Background(), sess.ID, "分析此项目", emit); err != nil {
		t.Fatalf("重试后应正常收尾: %v", err)
	}
	if retried != 1 {
		t.Errorf("应透出一轮「正在重试」，实际 %d 次", retried)
	}
	if done != 1 || !strings.Contains(reply.String(), "完整的分析结论") {
		t.Errorf("应正常收尾并给出正文：done=%d reply=%q", done, reply.String())
	}
}

func TestPersistentEmptyTurnAbortsVisibly(t *testing.T) {
	cases := []struct {
		name    string
		events  []llm.StreamEvent
		wantMsg string
	}{
		{
			name:    "彻底空返回",
			events:  []llm.StreamEvent{stopped()},
			wantMsg: "返回空内容",
		},
		{
			name:    "只回思考不回收",
			events:  []llm.StreamEvent{{Type: llm.EventReasoningDelta, Text: "想想想"}, stopped()},
			wantMsg: "只返回思考",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			evs := c.events
			p := &scriptProvider{scripts: []func() <-chan llm.StreamEvent{func() <-chan llm.StreamEvent {
				return streamOf(evs...)
			}}}
			a := newEmitTestAgent(t, p)
			sess, err := a.History().Create("", c.name)
			if err != nil {
				t.Fatal(err)
			}
			done := 0
			err = a.Run(context.Background(), sess.ID, "继续", func(ev Event) {
				if ev.Type == EventDone {
					done++
				}
			})
			if err == nil {
				t.Fatal("连续空回合必须报错中止，不能当成答完了")
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("报错应含 %q，实际：%v", c.wantMsg, err)
			}
			if done != 0 {
				t.Errorf("中止时不该发 done 事件（前端会显示成正常收尾）")
			}
		})
	}
}

// 有产出的那一轮要把计数清零：长任务里零星出现一两次空回合不该攒成中止。
func TestEmptyTurnCountResetsOnProductiveTurn(t *testing.T) {
	tool := func() <-chan llm.StreamEvent {
		return streamOf(
			llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "c1", ToolName: "missing_test_tool"},
			llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "c1", InputDelta: "{}"},
			stopped())
	}
	empty := func() <-chan llm.StreamEvent { return streamOf(stopped()) }
	// 工具 → 空 → 工具 → 空 → … 交替：每两次之间有产出，计数被清零，永远攒不到中止。
	scripts := make([]func() <-chan llm.StreamEvent, 0, 3)
	for i := 0; i < 3; i++ {
		scripts = append(scripts, tool, empty)
	}
	p := &scriptProvider{scripts: scripts}
	a := newEmitTestAgent(t, p)
	a.SetMaxSteps(6)
	sess, err := a.History().Create("", "交替")
	if err != nil {
		t.Fatal(err)
	}
	err = a.Run(context.Background(), sess.ID, "跑起来", func(Event) {})
	if err != nil && strings.Contains(err.Error(), "空内容") {
		t.Errorf("有产出的轮次应清空空回合计数，实际报错：%v", err)
	}
}
