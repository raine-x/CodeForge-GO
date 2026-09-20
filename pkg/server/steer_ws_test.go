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

// steerProvider 前两轮都返回一次工具调用（让循环继续跑、留下步骤边界），
// 第三轮回纯文本收尾。测试在第一个 tool_call 到达时插一条转向指令。
type steerProvider struct{ calls int }

func (p *steerProvider) Name() string { return "steer-stub" }

func (p *steerProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	p.calls++
	ch := make(chan llm.StreamEvent, 4)
	go func() {
		defer close(ch)
		if p.calls <= 2 {
			ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "t" + string(rune('0'+p.calls)),
				ToolName: "read_file"}
			ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "t" + string(rune('0'+p.calls)),
				InputDelta: `{"path":"missing.txt"}`}
			ch <- llm.StreamEvent{Type: llm.EventMessageStop}
			return
		}
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "已按新方向收尾"}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

func steerDial(t *testing.T, ts *httptest.Server, jar *cookiejar.Jar) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{Jar: jar}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("WebSocket 握手失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var first map[string]any
	if err := conn.ReadJSON(&first); err != nil || first["type"] != "ready" {
		t.Fatalf("首帧期望 ready：%v / %v", first, err)
	}
	return conn
}

func steerJar(t *testing.T, ts *httptest.Server) *cookiejar.Jar {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	resp, err := (&http.Client{Jar: jar}).Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return jar
}

// 运行中插话：先收到入队回执，再在某个步骤边界收到「已并入」事件；
// 且任务不被重启 —— 三个步骤都还在同一轮里跑完。
func TestSteerWhileRunningIsInjected(t *testing.T) {
	deps := newTestDepsAtProvider(t, "", &steerProvider{})
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))
	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": "先做 A"}); err != nil {
		t.Fatal(err)
	}

	var sessionID string
	queued, merged, idleSeen, textSeen := false, false, false, false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !idleSeen {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		switch ev["type"] {
		case "session":
			sessionID = toString(ev["session_id"])
		case "tool_call":
			if !queued { // 任务还在跑：此刻插话应当进队列，而不是另起一轮
				if err := conn.WriteJSON(map[string]any{
					"type": "steer", "session_id": sessionID, "text": "改成做 B"}); err != nil {
					t.Fatal(err)
				}
			}
		case "steer_queued":
			queued = true
		case "steer":
			merged = true
			if !strings.Contains(toString(ev["text"]), "改成做 B") {
				t.Errorf("steer 事件应带回归并的指令原文，实际 %v", ev["text"])
			}
		case "text":
			textSeen = true
		case "idle":
			idleSeen = true
		case "error":
			t.Fatalf("轮内报错: %v", ev["error"])
		}
	}
	if !queued || !merged {
		t.Fatalf("转向未走通：queued=%v merged=%v", queued, merged)
	}
	// 关键不变量：转向之后同一轮继续跑到正常收尾（text + idle），
	// 而不是被重启成一新一轮。步骤数不钉死 —— 指令落在哪个边界
	// 取决于插话到达的时机，可能是某步的开头也可能是收尾前的最后一次盘点。
	if !textSeen {
		t.Error("并入转向后本轮没有继续给出回复，任务像是被打断了")
	}
}

// 空闲时的 steer 不能压在队列里等一个不会来的步骤边界：按普通消息起一轮。
func TestSteerWhenIdleStartsNormalRun(t *testing.T) {
	deps := newTestDepsAtProvider(t, "", &steerProvider{})
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))
	if err := conn.WriteJSON(map[string]any{"type": "steer", "text": "没事先跑一下"}); err != nil {
		t.Fatal(err)
	}
	var sawBusy, sawQueued bool
	for i := 0; i < 12; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatal(err)
		}
		switch ev["type"] {
		case "busy":
			sawBusy = true
		case "steer_queued":
			sawQueued = true
		case "tool_call":
			return // 已经作为普通一轮跑起来了
		}
		if sawBusy {
			return
		}
	}
	if !sawBusy || sawQueued {
		t.Errorf("空闲时的 steer 应起一轮普通对话：busy=%v queued=%v", sawBusy, sawQueued)
	}
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}
