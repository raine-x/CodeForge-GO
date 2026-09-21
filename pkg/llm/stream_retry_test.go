package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 用 Hijack 精确制造「流被中途截断」：先声明一个比实际内容更长的 Content-Length，
// 写一小段后立刻关连接。客户端读到 body 长度对不上 → io.ErrUnexpectedEOF。
//
// 这是 2026-09-21 实测那次故障的等价复现（SSE 长连接被 VPN/代理掐断）。
func cutStream(w http.ResponseWriter, prelude string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("测试服务器不支持 Hijack")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		panic(err)
	}
	fmt.Fprint(buf, "HTTP/1.1 200 OK\r\n")
	fmt.Fprint(buf, "Content-Type: text/event-stream\r\n")
	fmt.Fprint(buf, "Content-Length: 99999\r\n") // 远大于实际写入量
	fmt.Fprint(buf, "\r\n")
	if prelude != "" {
		fmt.Fprint(buf, prelude)
	}
	buf.Flush()
	conn.Close() // 提前断开 → 长度对不上
}

func newTestProvider(url string) *OpenAIProvider {
	return &OpenAIProvider{
		baseURL: url,
		apiKey:  "test-key",
		model:   "test-model",
		// 用 fixed + 极短间隔，测试别真等 1 秒
		retry: RetryPolicy{MaxAttempts: 2, Mode: "fixed", Backoff: time.Millisecond},
	}
}

func drain(ch <-chan StreamEvent) []StreamEvent {
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

func hasText(evs []StreamEvent, want string) bool {
	for _, ev := range evs {
		if ev.Type == EventTextDelta && strings.Contains(ev.Text, want) {
			return true
		}
	}
	return false
}

func firstError(evs []StreamEvent) string {
	for _, ev := range evs {
		if ev.Type == EventError {
			return ev.Error
		}
	}
	return ""
}

// 一个字都还没产出就断了 → 必须自动重试，且对上层完全无感（不出现 EventError）。
func TestStreamRetriesWhenCutBeforeAnyContent(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			// 第一次：只发心跳注释，还没吐任何正文就断
			cutStream(w, ": ping\n\n")
			return
		}
		// 第二次：完整一轮
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream 不应同步失败: %v", err)
	}
	evs := drain(ch)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("应重试一次（共 2 次请求），实际 %d 次", got)
	}
	if !hasText(evs, "你好") {
		t.Errorf("重试后应拿到正文，实际事件: %+v", evs)
	}
	if msg := firstError(evs); msg != "" {
		t.Errorf("无感重试不该对上层报错，实际: %s", msg)
	}
}

// 已经吐出正文后才断 → **不能重试**（会重复输出），必须如实报错。
func TestStreamDoesNotRetryAfterContentEmitted(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 先发一段正文，再断
		cutStream(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半句话\"}}]}\n\n")
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream 不应同步失败: %v", err)
	}
	evs := drain(ch)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("已产出内容时不该重试，实际请求 %d 次", got)
	}
	if !hasText(evs, "半句话") {
		t.Errorf("已收到的正文不该丢，实际事件: %+v", evs)
	}
	msg := firstError(evs)
	if msg == "" {
		t.Fatal("流被截断应报错")
	}
	// 关键：不能把标准库原文抛给用户
	if strings.Contains(msg, "unexpected EOF") {
		t.Errorf("不应把标准库原文直接给用户，实际: %s", msg)
	}
	if !strings.Contains(msg, "传到一半") {
		t.Errorf("应说明是中途断开，实际: %s", msg)
	}
	if !strings.Contains(msg, "重试") {
		t.Errorf("应给出处置建议（重试），实际: %s", msg)
	}
}

// 重试次数用尽后，报错必须说明「已重试 N 次」，而不是让人以为只试了一次。
func TestStreamReportsAttemptsWhenExhausted(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		cutStream(w, ": ping\n\n")
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream 不应同步失败: %v", err)
	}
	evs := drain(ch)

	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("应尝试 2 次，实际 %d 次", got)
	}
	msg := firstError(evs)
	if !strings.Contains(msg, "已重试") {
		t.Errorf("用尽重试后应说明重试次数，实际: %s", msg)
	}
}

// 被取消时绝不重试 —— 用户点了停止还自动重试是最招人烦的行为。
//
// ⚠️ 取消必须发生在**响应头已到达之后**（POST 已返回、正在读流时）。
// 若在 POST 完成前取消，Stream 会同步返回 context canceled，测的就不是重试逻辑了
// （第一版就踩了这个坑）。
func TestStreamDoesNotRetryWhenCanceled(t *testing.T) {
	var hits int32
	release := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			panic("测试服务器不支持 Hijack")
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			panic(err)
		}
		fmt.Fprint(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n")
		fmt.Fprint(buf, ": ping\n\n")
		buf.Flush() // 响应头发出 → 客户端 postJSON 返回
		<-release   // 挂住，等测试取消
		conn.Close()
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream 不应同步失败: %v", err)
	}
	cancel()       // 此时正在读流
	close(release) // 让 handler 收尾
	drain(ch)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("取消后不该重试，实际请求 %d 次", got)
	}
}

// 不可重试的错误（如鉴权失败）不该浪费额度重试。
func TestStreamDoesNotRetryNonRetryable(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 正常返回一个鉴权错误块（语义性失败，非传输故障）
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"invalid api key\"}}\n\n")
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream 不应同步失败: %v", err)
	}
	evs := drain(ch)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("语义性失败不该重试，实际请求 %d 次", got)
	}
	if msg := firstError(evs); !strings.Contains(msg, "invalid api key") {
		t.Errorf("上游错误原文应如实透出（它本身已是人话），实际: %s", msg)
	}
}

// 正常流不该被这套兜底改动影响。
func TestStreamNormalPathUnaffected(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"完整回答\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ch, err := newTestProvider(srv.URL).Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	evs := drain(ch)

	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("正常流应只请求一次，实际 %d 次", got)
	}
	if !hasText(evs, "完整回答") {
		t.Errorf("应拿到正文，实际: %+v", evs)
	}
	if msg := firstError(evs); msg != "" {
		t.Errorf("正常流不该报错，实际: %s", msg)
	}
	var stopped bool
	for _, ev := range evs {
		if ev.Type == EventMessageStop {
			stopped = true
		}
	}
	if !stopped {
		t.Error("正常流应发 MessageStop")
	}
}

// 保证测试用的构造方式本身没写错（避免上面的用例因配置问题而假通过）。
func TestTestProviderPolicyIsSane(t *testing.T) {
	p := newTestProvider("http://127.0.0.1:1")
	if p.retry.MaxAttempts < 2 {
		t.Fatal("测试策略至少要有 2 次尝试，否则测不出重试")
	}
	if errors.Is(context.Canceled, nil) {
		t.Fatal("占位断言")
	}
}
