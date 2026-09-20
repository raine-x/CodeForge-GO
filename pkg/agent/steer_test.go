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
