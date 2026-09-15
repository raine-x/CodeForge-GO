package llm

import (
	"context"
	"strings"
	"testing"

	"codeforge/config"
)

// 用量统计（EventUsage）的解析测试。
//
// 上下文进度条明细要展示「已使用总 / 缓存命中 / 缓存未命中」，数据只能来自
// 上游流式响应里的 usage 字段 —— 两家协议位置完全不同（Anthropic 在
// message_start / message_delta，OpenAI 在末块 usage），这里各钉一条：
// 解析丢失时上层只会静默显示 0，没有报错可循。

// runAnthropicConsume 同步执行 Anthropic consume 并收集事件。
func runAnthropicConsume(t *testing.T, body string) []StreamEvent {
	t.Helper()
	p := &AnthropicProvider{}
	ch := make(chan StreamEvent, 64)
	p.consume(context.Background(), strings.NewReader(body), ch)
	close(ch)
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// findUsage 取最后一条 EventUsage 的负载。
func findUsage(evs []StreamEvent) *Usage {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == EventUsage {
			return evs[i].Usage
		}
	}
	return nil
}

// Anthropic：message_start 带输入用量（缓存三段），message_delta 带输出用量；
// 输入总数 = 未缓存 + 写缓存 + 读缓存，缓存命中 = 读缓存。
func TestAnthropicConsumeReportsUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_creation_input_tokens":20,"cache_read_input_tokens":500,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	u := findUsage(runAnthropicConsume(t, body))
	if u == nil {
		t.Fatal("流结束应发出 EventUsage（message_start 已带 usage）")
	}
	if u.InputTokens != 620 {
		t.Errorf("InputTokens = %d，期望 620（100 未缓存 + 20 写缓存 + 500 读缓存）", u.InputTokens)
	}
	if u.CachedTokens != 500 {
		t.Errorf("CachedTokens = %d，期望 500（cache_read_input_tokens）", u.CachedTokens)
	}
	if u.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d，期望 42（message_delta 的最终值）", u.OutputTokens)
	}
}

// OpenAI：include_usage=true 时用量在末块（choices 为空），cached 在
// prompt_tokens_details.cached_tokens。
func TestOpenAIConsumeReportsUsage(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		``,
		`data: {"choices":[],"usage":{"prompt_tokens":620,"completion_tokens":42,"total_tokens":662,"prompt_tokens_details":{"cached_tokens":500}}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	u := findUsage(runConsume(t, body))
	if u == nil {
		t.Fatal("流结束应发出 EventUsage（末块已带 usage）")
	}
	if u.InputTokens != 620 {
		t.Errorf("InputTokens = %d，期望 620（prompt_tokens）", u.InputTokens)
	}
	if u.CachedTokens != 500 {
		t.Errorf("CachedTokens = %d，期望 500（prompt_tokens_details.cached_tokens）", u.CachedTokens)
	}
	if u.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d，期望 42（completion_tokens）", u.OutputTokens)
	}
}

// 上游没下发 usage（未开 include_usage 的兼容网关）时不得发 EventUsage，
// 上层显示 0 即可，不能发零值事件冒充真实统计。
func TestOpenAIConsumeNoUsageNoEvent(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	for _, ev := range runConsume(t, body) {
		if ev.Type == EventUsage {
			t.Fatal("上游未下发 usage 时不应发出 EventUsage")
		}
	}
}

// 请求体必须带 stream_options.include_usage，否则 OpenAI 不会在流里下发 usage。
func TestOpenAIPayloadRequestsUsage(t *testing.T) {
	p := NewOpenAI(config.LLMConfig{})
	payload := p.buildPayload(Request{})
	so, ok := payload["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("payload 应含 stream_options.include_usage=true，实际 %v", payload["stream_options"])
	}
}
