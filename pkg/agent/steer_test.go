package agent

import (
	"context"
	"strings"
	"testing"

	"codeforge/pkg/llm"
)

// 中途转向（steering）：运行中收到的新指令在步骤边界并入，不丢已产生的工具结果。

func userTexts(msgs []llm.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role != llm.RoleUser {
			continue
		}
		var sb strings.Builder
		for _, b := range m.Content {
			if b.Type == llm.BlockText {
				sb.WriteString(b.Text)
			}
		}
		if sb.Len() > 0 {
			out = append(out, sb.String())
		}
	}
	return out
}

func TestSteerQueueSemantics(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return nil, nil
	}))

	// 没有运行中的循环：不能把指令压在队列里。
	if got := a.Steer("s1", "换个思路"); got != SteerIdle {
		t.Errorf("未开跑时应返回 SteerIdle，实际 %v", got)
	}

	a.beginRun("s1")
	for i := 0; i < maxSteerPending; i++ {
		if got := a.Steer("s1", "指令"); got != SteerQueued {
			t.Fatalf("第 %d 条应入队，实际 %v", i+1, got)
		}
	}
	if got := a.Steer("s1", "挤不进去了"); got != SteerFull {
		t.Errorf("超过 %d 条应返回 SteerFull，实际 %v", maxSteerPending, got)
	}
	if got := a.Steer("s2", "别的会话没在跑"); got != SteerIdle {
		t.Errorf("按会话隔离失效，实际 %v", got)
	}

	a.endRun("s1")
	if got := a.Steer("s1", "再来"); got != SteerIdle {
		t.Errorf("收跑后应回到 SteerIdle，实际 %v", got)
	}
	// 开跑前清残留：上一轮被取消时压在队列里的指令不得渗到下一轮。
	a.beginRun("s3")
	a.Steer("s3", "压在队列里")
	a.endRun("s3")
	a.beginRun("s3")
	defer a.endRun("s3")
	if got := a.drainSteer("s3"); len(got) != 0 {
		t.Errorf("beginRun 未清空残留队列: %v", got)
	}
}

// 工具跑完 → 用户中途插话 → 指令必须出现在下一次请求里，且排在工具结果之后。
func TestSteerLandsAfterToolResult(t *testing.T) {
	calls := 0
	var secondReq []string
	a := newEmitTestAgent(t, maxStepsProvider(func(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
		calls++
		if calls == 1 {
			return maxStepsToolStream(), nil // 一次工具调用，制造步骤边界
		}
		secondReq = userTexts(req.Messages)
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "好，改看 README"}
		close(ch)
		return ch, nil
	}))
	sess, err := a.History().Create("", "steer")
	if err != nil {
		t.Fatal(err)
	}

	// 在「工具结果刚回来」这一刻插话：等价于用户在模型干活时按了发送。
	var steerEvents int
	emit := func(ev Event) {
		if ev.Type == EventToolResult && steerEvents == 0 {
			if got := a.Steer(sess.ID, "先别改测试，改成只更新 README"); got != SteerQueued {
				t.Errorf("运行中插话应入队，实际 %v", got)
			}
		}
		if ev.Type == EventSteer {
			steerEvents++
		}
	}
	if err := a.Run(context.Background(), sess.ID, "把拼豆功能补上", emit); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("应跑两步（工具 + 收尾），实际 %d", calls)
	}
	if steerEvents != 1 {
		t.Errorf("应下发 1 条 steer 事件，实际 %d", steerEvents)
	}
	if !containsStr(secondReq, "先别改测试，改成只更新 README") {
		t.Errorf("第二次请求里没有并入转向指令: %q", secondReq)
	}
	// 原始提问还在，且顺序是「原提问 → 转向指令」。
	first := strings.Index(strings.Join(secondReq, "\n"), "把拼豆功能补上")
	second := strings.Index(strings.Join(secondReq, "\n"), "先别改测试")
	if first < 0 || second < first {
		t.Errorf("转向指令应排在原提问之后: %q", secondReq)
	}
	// 已产生的工具结果必须留在上下文里（转向的价值就在于此）。
	var hasToolResult bool
	for _, m := range sess.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				hasToolResult = true
			}
		}
	}
	if !hasToolResult {
		t.Error("工具结果被丢了，模型下一步无从知道刚跑过什么")
	}
	// 落库的要与内存一致：刷新页面后转向轨迹不能消失。
	loaded, ok := a.History().Get(sess.ID)
	if !ok || len(loaded.Messages) != len(sess.Messages) {
		t.Fatalf("会话未落库: ok=%v", ok)
	}
	if !containsStr(userTexts(loaded.Messages), "先别改测试，改成只更新 README") {
		t.Error("转向指令未持久化")
	}
}

// 模型正在生成「最后一句回答」时收到转向：不能直接收摊把话吞了。
func TestSteerAtTurnEndContinues(t *testing.T) {
	calls := 0
	sid := ""
	var a *Agent
	a = newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		calls++
		if calls == 1 {
			// 模拟：这一轮模型已经不再调工具、正要收尾，用户此刻按下发送。
			if got := a.Steer(sid, "等一下，再加个开关"); got != SteerQueued {
				t.Errorf("收尾前插话应入队，实际 %v", got)
			}
		}
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "回答"}
		close(ch)
		return ch, nil
	}))
	sess, err := a.History().Create("", "steer-end")
	if err != nil {
		t.Fatal(err)
	}
	sid = sess.ID
	done := 0
	if err := a.Run(context.Background(), sess.ID, "开始", func(ev Event) {
		if ev.Type == EventDone {
			done++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("收尾前收到转向应继续跑一轮，实际请求次数 %d", calls)
	}
	if done != 1 {
		t.Errorf("最终仍应正常收尾一次，done=%d", done)
	}
	if !containsStr(userTexts(sess.Messages), "等一下，再加个开关") {
		t.Errorf("转向指令未并入历史: %q", userTexts(sess.Messages))
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// steer 消息的身份标记（Origin）
//
// 插话与「真正的提问」在历史里长得一样，导致三处误判：重新生成 / 编辑重发
// 会把插话当成最后一条提问，界面回放也把插话渲染成普通提问。
// ---------------------------------------------------------------------------

func steerText(s string) llm.Message {
	m := llm.TextMessage(llm.RoleUser, s)
	m.Origin = OriginSteer
	return m
}

// 定位函数必须跳过插话：只认真正的提问。
func TestUserQuestionLocatorsSkipSteer(t *testing.T) {
	msgs := []llm.Message{
		userText("q1"), assistantText("a1"),
		steerText("顺便改 b"), assistantText("a2"),
	}
	if got := lastUserQuestionIndex(msgs); got != 0 {
		t.Errorf("最后一条提问应为 q1（下标 0），实际 %d", got)
	}
	if got := lastPlainUserIndex(msgs); got != 2 {
		t.Errorf("压缩切点口径不变：最后一条纯文本发言仍是插话（下标 2），实际 %d", got)
	}
	if got := nthLastUserQuestionIndex(msgs, 0); got != 0 {
		t.Errorf("n=0 期望 0，得到 %d", got)
	}
	if got := nthLastUserQuestionIndex(msgs, 1); got != -1 {
		t.Errorf("插话不计数，n=1 应越界为 -1，得到 %d", got)
	}
}

// 可编辑白名单不得包含插话：前端给它挂编辑按钮没有意义，
// 且 back 序号必须与 RerunFrom 同源，否则点第 back 条会改错消息。
func TestEditableUserMessagesExcludesSteer(t *testing.T) {
	a, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("q1"), assistantText("a1"),
		steerText("顺便改 b"), assistantText("a2"),
	})
	got := a.EditableUserMessages(sess.ID, 5)
	if len(got) != 1 {
		t.Fatalf("插话不该出现在可编辑白名单里，实际 %d 条：%+v", len(got), got)
	}
	if got[0].Text != "q1" || got[0].Back != 0 {
		t.Errorf("白名单应只剩 q1（back=0），实际 %+v", got[0])
	}
}

// 「重新生成」应回到真正的提问，而不是落到中途插入的指令上。
func TestRegenerateTargetsLastQuestionNotSteer(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return okStream("答"), nil
	}))
	sess, err := a.History().Create("", "转向后重新生成")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = []llm.Message{
		userText("重构 X"), assistantText("好"),
		steerText("顺便改 b"), assistantText("已改 b"),
	}

	if err := a.Regenerate(context.Background(), sess.ID, func(Event) {}); err != nil {
		t.Fatalf("重新生成失败: %v", err)
	}
	_, msgs := sess.SnapshotForRender()
	if len(msgs) != 2 {
		t.Fatalf("应从提问重跑（插话一并丢弃），实际 %d 条：%+v", len(msgs), msgs)
	}
	if msgs[0].Content[0].Text != "重构 X" || msgs[1].Content[0].Text != "答" {
		t.Fatalf("历史不符：%+v", msgs)
	}
}

// 插话经 appendSteers 并入时必须带上标记（它是标记的唯一写入口）。
func TestAppendSteersMarksOrigin(t *testing.T) {
	a, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("q1"), assistantText("a1")})
	a.appendSteers(sess, []string{"换个思路"}, func(Event) {})

	msgs := sess.Messages
	if len(msgs) != 3 {
		t.Fatalf("应并入一条消息，实际 %d 条", len(msgs))
	}
	if msgs[2].Origin != OriginSteer {
		t.Errorf("插话必须带 OriginSteer 标记，实际 %q", msgs[2].Origin)
	}
	if msgs[2].Content[0].Text != "换个思路" {
		t.Errorf("内容不得被改写，实际 %q", msgs[2].Content[0].Text)
	}
}
