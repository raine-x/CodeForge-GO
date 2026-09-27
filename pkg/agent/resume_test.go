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
// assistant / tool_use / tool_result 全部从历史里删掉。打断之后该做的是**接着跑**。
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

	// 点「继续」＝ Agent.ContinueTurn：从断点续跑，不追加任何用户消息。
	if err := a.ContinueTurn(context.Background(), sess.ID, func(Event) {}); err == nil {
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

// 「继续」不追加用户消息。
//
// 旧实现是前端发一句字面量「继续」当普通用户消息，那会在历史里留下一条用户
// 从未说过的假提问：历史回放时它仍在，而且模型分不清「被打断后接着跑」
// 与「用户新提了一个要求」，容易把做完的再做一遍。
func TestContinueTurnAppendsNoUserMessage(t *testing.T) {
	var seen []llm.Request
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		seen = append(seen, req)
		return maxStepsToolStream(), nil
	}))
	a.SetMaxSteps(1)

	sess, err := a.History().Create("", "no-fake")
	if err != nil {
		t.Fatal(err)
	}
	// max_steps=1，第一轮跑满即返回「达到上限」错误 —— 这正是「被打断/跑满」
	// 的形状，「继续」要接的正是这种会话。
	_ = a.Run(context.Background(), sess.ID, "原始问题", func(Event) {})
	before := len(seen)
	if before == 0 {
		t.Fatal("第一轮没有发出请求")
	}

	seen = nil
	if err := a.ContinueTurn(context.Background(), sess.ID, func(Event) {}); err == nil {
		t.Fatal("应当跑满 max_steps")
	}
	if len(seen) == 0 {
		t.Fatal("「继续」没有发出任何请求")
	}
	// 送模的消息里不得凭空多出一条用户提问。
	first := seen[0]
	users := 0
	for _, m := range first.Messages {
		if m.Role == llm.RoleUser && isPlainUserText(m) {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("「继续」后送模的用户提问应仍只有 1 条（原始问题），实际 %d", users)
	}
	for _, m := range first.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockText && strings.TrimSpace(b.Text) == "继续" {
				t.Fatal("「继续」不该作为用户消息进入上下文")
			}
		}
	}
	// 「继续」的语义改由一次性系统提示承载。
	if !strings.Contains(first.System, "本轮是「继续上一轮」") {
		t.Fatal("「继续」的系统提示里应说明这是接着上一轮跑")
	}
}

// 「继续」提示只在本次运行内有效：跑完就清空，不留在会话上。
func TestContinueTurnHintIsTransient(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	a.SetMaxSteps(1)
	sess, err := a.History().Create("", "transient")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), sess.ID, "问题", func(Event) {}); err == nil {
		t.Fatal("应当跑满 max_steps")
	}
	_ = a.ContinueTurn(context.Background(), sess.ID, func(Event) {})
	got, ok := a.History().Get(sess.ID)
	if !ok {
		t.Fatal("读不到会话")
	}
	if h := got.ContinueHint(); h != "" {
		t.Fatalf("跑完后「继续」提示应已清空，实际还挂着 %q", h)
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

// 纯文本阶段打断时，「继续」圆环必须挂得出来。
//
// 这是 2026-09-26 反馈对应的真实故障：用户在模型**流式吐字时**按 Esc（最常见的
// 打断姿势），recordPartialTurn 落下一条**纯文本**助手消息 —— 形状与真终稿完全
// 一样，于是 UnfinishedTurnAnchor 判成「已完成」，圆环永远不出现。
// 带工具调用的半截轮次本来就因 hasToolUse 而判为未完成，所以只有纯文本这条路是漏的。
func TestUnfinishedTurnAnchorSeesTextOnlyPartialTurn(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	sess, err := a.History().Create("", "partial")
	if err != nil {
		t.Fatal(err)
	}
	sess.appendMessages(llm.TextMessage(llm.RoleUser, "问题"))
	a.recordPartialTurn(sess, &llm.AssistantTurn{Text: "我先看一下…然后就"}, "本轮被用户打断")
	a.save(sess, true) // 落盘：判据要能在页面刷新 / 进程重启后算出来

	if got := a.UnfinishedTurnAnchor(sess.ID); got != 0 {
		t.Fatalf("纯文本被打断应判为未完成（back=0，圆环挂出来），实际 %d", got)
	}
	// 标记必须落盘：判据要能在页面刷新 / 进程重启后算出来。
	reloaded, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok {
		t.Fatal("重载会话失败")
	}
	if len(reloaded.Messages) == 0 {
		t.Fatal("重载后会话没有消息")
	}
	last := reloaded.Messages[len(reloaded.Messages)-1]
	if last.Origin != OriginPartial {
		t.Errorf("被截断的一轮必须带 OriginPartial 标记（要落盘），实际 %q", last.Origin)
	}
	if got := a.UnfinishedTurnAnchor(sess.ID); got != 0 {
		t.Fatalf("重载后仍应判为未完成，实际 %d", got)
	}
}

// 真正给出终稿（纯文本、无工具调用、未被截断）时，不该挂圆环。
//
// 这是与上面那条配对的反例，防止「修 bug 修成永远都提示继续」。
func TestUnfinishedTurnAnchorIgnoresFinalTextAnswer(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	sess, err := a.History().Create("", "final")
	if err != nil {
		t.Fatal(err)
	}
	sess.appendMessages(llm.TextMessage(llm.RoleUser, "问题"))
	sess.appendMessages(llm.AssistantBlocksMessage([]llm.ContentBlock{
		{Type: llm.BlockText, Text: "已经查完了，结论是这样。"},
	}))
	if got := a.UnfinishedTurnAnchor(sess.ID); got != -1 {
		t.Fatalf("完整终稿应判为已完成（-1，不挂圆环），实际 %d", got)
	}
}

// 圆环挂出来之后点它，模型要能看到「这是半截的」并接着写。
func TestContinueTurnAfterTextInterrupt(t *testing.T) {
	var seen []llm.Request
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		seen = append(seen, req)
		return maxStepsToolStream(), nil
	}))
	a.SetMaxSteps(1)
	sess, err := a.History().Create("", "resume-text")
	if err != nil {
		t.Fatal(err)
	}
	sess.appendMessages(llm.TextMessage(llm.RoleUser, "把说明补全"))
	a.recordPartialTurn(sess, &llm.AssistantTurn{Text: "## 说明\n这是开头"}, "本轮被用户打断")

	seen = nil
	_ = a.ContinueTurn(context.Background(), sess.ID, func(Event) {})
	if len(seen) == 0 {
		t.Fatal("「继续」没有发出请求")
	}
	last := seen[len(seen)-1]
	if !strings.Contains(last.System, "本轮是「继续上一轮」") {
		t.Error("系统提示应说明这是接着上一轮跑")
	}
	// 半截的那段正文必须原样还在上下文里（模型要接着它写，不是从头再来）。
	found := false
	for _, m := range last.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockText && strings.Contains(b.Text, "这是开头") {
				found = true
			}
		}
	}
	if !found {
		t.Error("半截正文应保留在上下文里，供模型接着写")
	}
}

// 同一句话连着问两次时，每条消息的编辑 back 必须各自正确、互不串位。
//
// 服务端本来就给对了（两条各有正确 back）；这条用例锁住的是「别为了省事
// 把 back 写死成按文本查」—— 早先前端用 Map<text, back>，重复文本时后写的
// 覆盖先写的，于是第一条的编辑按钮也拿到最后那条的 back，点下去改错消息。
func TestEditableUserMessagesKeepDuplicateTextApart(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	sess, err := a.History().Create("", "dup")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		sess.appendMessages(llm.TextMessage(llm.RoleUser, "继续排查这个问题"))
		sess.appendMessages(llm.AssistantBlocksMessage([]llm.ContentBlock{
			{Type: llm.BlockText, Text: "第 " + string(rune('A'+i)) + " 次回答"},
		}))
	}
	refs := a.EditableUserMessages(sess.ID, 3)
	if len(refs) != 2 {
		t.Fatalf("应下发 2 条可编辑白名单，实际 %d", len(refs))
	}
	// 正序：最后一条 back=0，倒数第二条 back=1。
	if refs[0].Back != 1 || refs[1].Back != 0 {
		t.Fatalf("重复文本时两条的 back 必须各自正确，实际 %+v", refs)
	}
	// 关键：两条 back 不同，前端按 back 定位才不会串位。
	if refs[0].Back == refs[1].Back {
		t.Fatal("两条消息的 back 不应相同")
	}
	// 且 RerunFrom 用同一个 back 序号能定位到正确的下标。
	first, _ := a.History().Get(sess.ID)
	if got := nthLastUserQuestionIndex(first.Messages, 1); got != refs[0].Index {
		t.Errorf("back=1 应定位到 index=%d，实际 %d", refs[0].Index, got)
	}
}
