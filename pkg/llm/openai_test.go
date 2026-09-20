package llm

import (
	"context"
	"strings"
	"testing"
)

// runConsume 同步执行 consume 并收集事件（consume 不负责关闭 channel，由这里收口）。
func runConsume(t *testing.T, body string) []StreamEvent {
	t.Helper()
	p := &OpenAIProvider{}
	ch := make(chan StreamEvent, 64)
	p.consume(context.Background(), strings.NewReader(body), ch)
	close(ch)
	var out []StreamEvent
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// 思考 + 回答耗尽 max_tokens 预算时，上游以 finish_reason=length 结束，
// 适配器必须显式报错，而不是静默当作正常完成。
func TestOpenAIConsumeReportsLengthTruncation(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"思考中…"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	hasErr := false
	for _, ev := range runConsume(t, body) {
		if ev.Type == EventError && strings.Contains(ev.Error, "max_tokens") {
			hasErr = true
		}
	}
	if !hasErr {
		t.Fatal("finish_reason=length 应产生 max_tokens 截断错误事件")
	}
}

func TestOpenAIConsumeEOF(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tail      string
		wantError bool
	}{
		{name: "incomplete", wantError: true},
		{name: "finish_without_done", tail: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"},
		{name: "done_without_finish", tail: "data: [DONE]\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := runConsume(t, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"+tc.tail)
			var errors, stops int
			for _, ev := range events {
				if ev.Type == EventError {
					errors++
				}
				if ev.Type == EventMessageStop {
					stops++
				}
			}
			if tc.wantError {
				if errors != 1 || stops != 0 {
					t.Fatalf("incomplete stream: errors=%d stops=%d", errors, stops)
				}
			} else if errors != 0 || stops != 1 {
				t.Fatalf("complete stream: errors=%d stops=%d", errors, stops)
			}
		})
	}
}

// 正常结束（stop）不应误报。
func TestOpenAIConsumeNormalStopNoError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"你好"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	for _, ev := range runConsume(t, body) {
		if ev.Type == EventError {
			t.Fatalf("正常 stop 不应产生错误事件，实际: %s", ev.Error)
		}
	}
}

// 上游把 function.name 拆在**后续** delta 里时，适配器必须补发一次 ToolUseStart。
//
// 背景（2026-09-19 实际故障）：OpenAI 兼容协议允许首个 tool_call delta 只带 id，
// name 稍后才到。适配器原本只在首个 delta 发一次 ToolUseStart（那时 name 是空的），
// 后续补 name 时只改了本地 map、没补发事件 —— 于是消费端（agent.go）拿不到名字，
// 落库的 tool_use 块缺 name。后果是前端回放该会话历史时 toolLabel 拿到 undefined 抛异常，
// 中断整次回放，侧栏高亮停在上一个会话（表现为「切过去没有选中态」）。
func TestOpenAIConsumeReemitsToolStartWhenNameArrivesLate(t *testing.T) {
	body := strings.Join([]string{
		// 首个 delta：只有 id，没有 name
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"arguments":""}}]}}]}`,
		``,
		// name 在第二个 delta 才到
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read_file"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var starts []string
	for _, ev := range runConsume(t, body) {
		if ev.Type == EventToolUseStart {
			starts = append(starts, ev.ToolName)
		}
	}
	if len(starts) < 2 {
		t.Fatalf("name 迟到时必须补发一次 ToolUseStart，实际只发了 %d 次: %q", len(starts), starts)
	}
	if last := starts[len(starts)-1]; last != "read_file" {
		t.Fatalf("补发的 ToolUseStart 应带真实工具名，实际 %q（全部: %q）", last, starts)
	}
}

// 反例保护：name 在首个 delta 就到位时，**不该**多补发（否则消费端会收到重复事件）。
func TestOpenAIConsumeDoesNotReemitWhenNameArrivesFirst(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"read_file","arguments":""}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var starts int
	for _, ev := range runConsume(t, body) {
		if ev.Type == EventToolUseStart {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("name 首个 delta 就到位时只应发 1 次 ToolUseStart，实际 %d 次", starts)
	}
}
