package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/llm"
)

// 「继续」按钮的 WS 契约：continue_turn 从断点续跑，**不在历史里追加用户消息**。
//
// 旧实现是前端发一句字面量「继续」当普通用户消息（user_message），那会在会话里
// 留下一条用户从未说过的假提问：历史回放时它仍在，而且模型分不清「被打断后接着跑」
// 与「用户新提了一个要求」。这条用例把「不追加用户消息」钉在协议层。

// continueProvider 记录每次收到的请求，供断言「送模上下文里没有多出用户提问」。
type continueProvider struct {
	requests []llm.Request
}

func (p *continueProvider) Name() string { return "continue-stub" }

func (p *continueProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "接着做完了"}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

func TestWSContinueTurnRunsWithoutFakeUserMessage(t *testing.T) {
	prov := &continueProvider{}
	deps := newTestDepsAtProvider(t, "", prov)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))
	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": "把活干完"}); err != nil {
		t.Fatal(err)
	}
	var sessionID string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		switch ev["type"] {
		case "session":
			sessionID = toString(ev["session_id"])
		case "error":
			t.Fatalf("首轮报错: %v", ev["error"])
		case "idle":
			if sessionID == "" {
				t.Fatal("首轮没有拿到 session_id")
			}
			if n := countUserQuestions(prov.requests[0]); n != 1 {
				t.Fatalf("首轮送模的用户提问应为 1 条，实际 %d", n)
			}
			goto continueRun
		}
	}
	t.Fatal("首轮没有结束")

continueRun:
	if err := conn.WriteJSON(map[string]any{
		"type": "continue_turn", "session_id": sessionID}); err != nil {
		t.Fatal(err)
	}
	sawText := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取「继续」事件失败: %v", err)
		}
		switch ev["type"] {
		case "text":
			sawText = true
		case "error":
			t.Fatalf("「继续」轮报错: %v", ev["error"])
		case "idle":
			goto done
		}
	}
	t.Fatal("「继续」轮没有结束")

done:
	if !sawText {
		t.Error("「继续」轮应给出回复")
	}
	if len(prov.requests) == 0 {
		t.Fatal("「继续」没有发出任何请求")
	}
	// 关键断言：「继续」不得往上下文里塞一条用户消息。
	last := prov.requests[len(prov.requests)-1]
	if n := countUserQuestions(last); n != 1 {
		t.Errorf("「继续」后送模的用户提问应仍只有 1 条，实际 %d（出现了假提问）", n)
	}
	for _, m := range last.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) == "继续" {
				t.Fatal("「继续」不该作为用户消息进入上下文")
			}
		}
	}
	if !strings.Contains(last.System, "本轮是「继续上一轮」") {
		t.Error("「继续」轮的系统提示应说明这是接着上一轮跑")
	}
}

// countUserQuestions 数送模请求里「用户纯文本提问」的条数。
//
// 只数文本块：工具结果回填也是 role=user，但带 tool_result，不是提问。
func countUserQuestions(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				continue
			}
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				n++
			}
		}
	}
	return n
}
