package agent

import (
	"context"
	"path/filepath"
	"testing"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// toolStreamProvider 先吐一段文本，再发出工具调用流（start + 慢速参数增量 + stop），
// 模拟「大参数工具调用生成中」的长流：tool_pending 必须在参数拼装期间就发出，
// 而不是等流结束后随 tool_call 一起到。
type pendingProvider struct{ events []llm.StreamEvent }

func (p *pendingProvider) Name() string { return "pending-stub" }

func (p *pendingProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, len(p.events)+2)
	go func() {
		defer close(ch)
		for _, ev := range p.events {
			ch <- ev
		}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// 无任何工具调用时不得误发 tool_pending。
func TestConsumeStreamNoPendingWithoutToolUse(t *testing.T) {
	a := newEmitTestAgent(t, &pendingProvider{events: []llm.StreamEvent{
		{Type: llm.EventTextDelta, Text: "纯文本回复"},
	}})
	sess := &Session{ID: "s-plain", Messages: []llm.Message{llm.TextMessage(llm.RoleUser, "hi")}}
	if _, err := a.consumeStream(context.Background(), sess, mustStream(t, a, sess), func(ev Event) {
		if ev.Type == EventToolPending {
			t.Errorf("无工具调用却发出 tool_pending：%+v", ev)
		}
	}); err != nil {
		t.Fatal(err)
	}
}

// 首个工具开始生成参数时立刻发出 tool_pending（只发一次，多工具不重复）。
func TestConsumeStreamEmitsPendingOnToolUseStart(t *testing.T) {
	a := newEmitTestAgent(t, &pendingProvider{events: []llm.StreamEvent{
		{Type: llm.EventToolUseStart, ToolUseID: "t1", ToolName: "write_file"},
		{Type: llm.EventToolUseDelta, ToolUseID: "t1", InputDelta: `{"path":"a`},
		{Type: llm.EventToolUseDelta, ToolUseID: "t1", InputDelta: `.html","content":"`},
		{Type: llm.EventToolUseStart, ToolUseID: "t2", ToolName: "edit_file"},
		{Type: llm.EventToolUseDelta, ToolUseID: "t2", InputDelta: `{}`},
	}})
	sess := &Session{ID: "s-pending", Messages: []llm.Message{llm.TextMessage(llm.RoleUser, "hi")}}
	var pendings []Event
	if _, err := a.consumeStream(context.Background(), sess, mustStream(t, a, sess), func(ev Event) {
		if ev.Type == EventToolPending {
			pendings = append(pendings, ev)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(pendings) != 1 {
		t.Fatalf("tool_pending 应只发一次，实际 %d 次：%+v", len(pendings), pendings)
	}
	if pendings[0].ToolName != "write_file" {
		t.Errorf("tool_pending 应携带工具名 write_file，实际 %q", pendings[0].ToolName)
	}
}

// mustStream 构造一个已填充的流通道供 consumeStream 消费（provider.Stream 已在 stub 内完成）。
func mustStream(t *testing.T, a *Agent, sess *Session) <-chan llm.StreamEvent {
	t.Helper()
	p := a.provider.(*pendingProvider)
	ch := make(chan llm.StreamEvent, len(p.events)+2)
	go func() {
		defer close(ch)
		for _, ev := range p.events {
			ch <- ev
		}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch
}

// 编译期依赖防呆：store/security 是 newEmitTestAgent 的间接依赖，引用一次防 import 漂移。
var (
	_ = store.Open
	_ = security.NewPolicy
	_ = tools.NewRegistry
	_ = filepath.Join
	_ = config.AgentConfig{}
)
