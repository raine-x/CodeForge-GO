package agent

import (
	"context"
	"strings"
	"testing"

	"codeforge/pkg/llm"
)

// 打断之后「继续」：历史只增不减，绝不删掉已经跑完的操作记录。
//
// 2026-09-22 反馈：跑满 max_steps 后点圆环，界面上的操作记录被一条错误覆盖、
// 库里的记录也被删。根因是那条路径走的是「截断重跑」（RerunFrom）——它把该轮的
// assistant / tool_use / tool_result 全部从历史里删掉。打断之后该做的是**接着跑**：
// 前端改发一句普通用户消息「继续」，服务端只追加、不截断。
//
// 这条用例把「只增不减」钉死：若哪天有人把「继续」又接回截断路径，它会立刻红。
func TestContinueAfterMaxStepsOnlyGrowsHistory(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	a.SetMaxSteps(3)

	sess, err := a.History().Create("", "continue")
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Run(context.Background(), sess.ID, "把活干完", func(Event) {}); err == nil {
		t.Fatal("第一轮应当跑满 max_steps")
	}
	before, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok {
		t.Fatal("读不到会话")
	}
	if len(before.Messages) != 7 {
		t.Fatalf("第一轮应落 7 条（提问 + 3×(助手+工具结果)），实际 %d", len(before.Messages))
	}

	// 点「继续」＝ 前端发一条普通用户消息，与用户手打一句「继续」完全同一条链路。
	if err := a.Run(context.Background(), sess.ID, "继续", func(Event) {}); err == nil {
		t.Fatal("第二轮也应跑满 max_steps")
	}
	after, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok {
		t.Fatal("读不到会话")
	}
	if len(after.Messages) <= len(before.Messages) {
		t.Fatalf("「继续」不得让历史变短：%d → %d", len(before.Messages), len(after.Messages))
	}
	// 第一轮的每一条都必须原样还在（不是「条数够了就行」）。
	if !equalPrefix(messageFingerprint(after.Messages), messageFingerprint(before.Messages)) {
		t.Errorf("「继续」之后第一轮的历史被改动了\nbefore=%v\nafter=%v",
			messageFingerprint(before.Messages), messageFingerprint(after.Messages))
	}
}

// 续跑前把末尾「没有结果的 tool_use」补上结果。
//
// 形状来源：工具执行到一半进程被杀 / 被强杀，历史末尾挂着没有配对的 tool_use。
// 上游对消息序列有硬约束，带着它请求会被 400 拒绝 —— 补一条说明性结果，
// 而不是把这条调用删掉（删掉就又变成「抹记录」了）。
func TestRepairDanglingToolUseBeforeResume(t *testing.T) {
	var sent []llm.Message
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		sent = req.Messages
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "接上了"}
		close(ch)
		return ch, nil
	}))
	sess, err := a.History().Create("", "dangling")
	if err != nil {
		t.Fatal(err)
	}

	// 直接构造残缺历史：末尾是 assistant 的 tool_use，没有对应的 tool_result。
	sess.Messages = append(sess.Messages,
		llm.TextMessage(llm.RoleUser, "改一下文件"),
		llm.AssistantBlocksMessage([]llm.ContentBlock{
			{Type: llm.BlockText, Text: "我来改"},
			{Type: llm.BlockToolUse, ID: "call_orphan", Name: "edit_file", Input: []byte(`{}`)},
		}),
	)

	var infos []string
	emit := func(ev Event) {
		if ev.Type == EventInfo {
			infos = append(infos, ev.Text)
		}
	}
	if err := a.Run(context.Background(), sess.ID, "继续", emit); err != nil {
		t.Fatalf("续跑不应失败：%v", err)
	}

	// ① 发给上游的请求里，那个孤儿 tool_use 必须有配对的结果，且紧跟在它后面。
	if !hasToolResultFor(sent, "call_orphan") {
		t.Errorf("上游请求里缺少 call_orphan 的结果\n请求=%v", messageFingerprint(sent))
	}
	if got := toolResultIndex(sent, "call_orphan"); got < 0 || got != toolUseIndex(sent, "call_orphan")+1 {
		t.Errorf("补上的结果应紧跟在 tool_use 之后，实际 tool_use@%d result@%d",
			toolUseIndex(sent, "call_orphan"), got)
	}
	// ② 要有一条 info 告知用户，而不是悄悄改动历史。
	if len(infos) == 0 {
		t.Error("应当有一条 info 事件说明补了记录")
	}
	// ③ 修复要落盘：这是历史的一部分，不是只对这一次请求生效。
	loaded, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok {
		t.Fatal("读不到会话")
	}
	if !hasToolResultFor(loaded.Messages, "call_orphan") {
		t.Error("补上的 tool_result 应当落盘")
	}
}

// 历史末尾没有残缺时不得凭空插入记录（避免「修复」变成噪音）。
func TestRepairDanglingToolUseNoopOnCleanHistory(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "好了"}
		close(ch)
		return ch, nil
	}))
	sess, err := a.History().Create("", "clean")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "在吗"))
	before := len(sess.Messages)

	infos := 0
	if err := a.Run(context.Background(), sess.ID, "继续", func(ev Event) {
		if ev.Type == EventInfo {
			infos++
		}
	}); err != nil {
		t.Fatalf("正常续跑不应失败：%v", err)
	}
	if infos != 0 {
		t.Error("历史完整时不应发修复提示")
	}
	if got := len(sess.Messages); got != before+2 { // +用户消息 +助手回复
		t.Errorf("历史完整时不应多插记录：%d → %d", before, got)
	}
}

// ---------------------------------------------------------------------------
// 断言辅助
// ---------------------------------------------------------------------------

// messageFingerprint 把消息序列压成可比较的字符串切片（角色 + 块类型 + 关键字段）。
func messageFingerprint(msgs []llm.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		var sb strings.Builder
		sb.WriteString(string(m.Role))
		for _, b := range m.Content {
			sb.WriteString("|")
			sb.WriteString(string(b.Type))
			sb.WriteString("#")
			sb.WriteString(b.ID)
			sb.WriteString(b.ToolUseID)
			sb.WriteString(":")
			sb.WriteString(b.Text)
			sb.WriteString(b.Content)
		}
		out = append(out, sb.String())
	}
	return out
}

func equalPrefix(got, want []string) bool {
	if len(got) < len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func hasToolResultFor(msgs []llm.Message, id string) bool {
	return toolResultIndex(msgs, id) >= 0
}

// toolUseIndex / toolResultIndex 返回该 id 首次出现所在的消息下标（找不到返回 -1）。
func toolUseIndex(msgs []llm.Message, id string) int {
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && b.ID == id {
				return i
			}
		}
	}
	return -1
}

func toolResultIndex(msgs []llm.Message, id string) int {
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				return i
			}
		}
	}
	return -1
}
