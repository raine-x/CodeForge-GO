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

// captureProvider 记录每次 LLM 请求，供断言「WS 参数是否真的进入了请求」。
type captureProvider struct {
	requests []llm.Request
}

func (p *captureProvider) Name() string { return "capture" }

func (p *captureProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "ok"}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// 思考强度链路：前端拉条的值必须原样进入 LLM 请求（WS thinking → Request.Thinking）。
// 这里只钉住「有没有传到」；参数如何落到厂商载荷由 pkg/llm 的测试覆盖。
func TestThinkingLevelReachesLLMRequest(t *testing.T) {
	provider := &captureProvider{}
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

	// 首帧 ready
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var first map[string]any
	if err := conn.ReadJSON(&first); err != nil || first["type"] != "ready" {
		t.Fatalf("首帧期望 ready：%v / %v", first, err)
	}

	send := func(thinking string) {
		t.Helper()
		msg := map[string]any{"type": "user_message", "text": "你好"}
		if thinking != "" {
			msg["thinking"] = thinking
		}
		if err := conn.WriteJSON(msg); err != nil {
			t.Fatalf("发送消息失败: %v", err)
		}
		for i := 0; i < 40; i++ {
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			var ev map[string]any
			if err := conn.ReadJSON(&ev); err != nil {
				t.Fatalf("读取事件失败: %v", err)
			}
			if ev["type"] == "idle" {
				return
			}
		}
		t.Fatal("未等到 idle 事件")
	}

	send("high")
	send("")

	if len(provider.requests) < 2 {
		t.Fatalf("期望至少 2 次 LLM 请求，实际 %d", len(provider.requests))
	}
	if got := provider.requests[0].Thinking; got != "high" {
		t.Errorf("第一轮 thinking 期望 high，实际 %q（拉条没有传到上游）", got)
	}
	if got := provider.requests[1].Thinking; got != "" {
		t.Errorf("第二轮未指定 thinking，期望空串，实际 %q", got)
	}
}
