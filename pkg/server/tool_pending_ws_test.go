package server

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/pkg/llm"
)

// turnProvider 第一轮发一个工具调用（start → 慢参数 → stop），
// 第二轮回纯文本，驱动完整的一轮「工具调用生成 → 执行 → 再回复」。
type turnProvider struct{ calls int }

func (p *turnProvider) Name() string { return "turn-stub" }

func (p *turnProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	p.calls++
	ch := make(chan llm.StreamEvent, 4)
	go func() {
		defer close(ch)
		if p.calls == 1 {
			ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "t1", ToolName: "read_file"}
			// 模拟大参数生成耗时：参数增量之间的延迟即「无反馈窗口」
			ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "t1", InputDelta: `{"path":"a.txt"}`}
			ch <- llm.StreamEvent{Type: llm.EventMessageStop}
			return
		}
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "完成"}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// 端到端：tool_pending 必须先于 tool_call 到达前端（大参数生成期间的实时反馈），
// 且 tool_call 之后正常走 tool_result → idle。
func TestToolPendingPrecedesToolCall(t *testing.T) {
	provider := &turnProvider{}
	deps := newTestDepsAtProvider(t, "", provider)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取 Cookie 失败: %v", err)
	}
	_ = resp.Body.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.Dialer{Jar: jar}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 握手失败: %v", err)
	}
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var first map[string]any
	if err := conn.ReadJSON(&first); err != nil || first["type"] != "ready" {
		t.Fatalf("首帧期望 ready：%v / %v", first, err)
	}

	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": "读文件"}); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}

	var order []string
	pendingIdx, callIdx := -1, -1
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		switch ev["type"] {
		case "tool_pending":
			if pendingIdx < 0 {
				pendingIdx = len(order)
				order = append(order, "tool_pending")
				if name, _ := ev["tool_name"].(string); name != "read_file" {
					t.Errorf("tool_pending 应携带工具名 read_file，实际 %v", ev["tool_name"])
				}
			} else {
				t.Errorf("tool_pending 只应发一次，收到第二次：%v", ev)
			}
		case "tool_call":
			callIdx = len(order)
			order = append(order, "tool_call")
		case "idle":
			goto done
		}
	}
	t.Fatal("未等到 idle 事件")
done:
	if pendingIdx < 0 {
		t.Fatalf("事件流中没有 tool_pending：%v", order)
	}
	if callIdx < 0 {
		t.Fatalf("事件流中没有 tool_call：%v", order)
	}
	if pendingIdx > callIdx {
		t.Errorf("tool_pending(%d) 必须先于 tool_call(%d) 到达：%v", pendingIdx, callIdx, order)
	}
	t.Logf("事件顺序：%v", order)
}
