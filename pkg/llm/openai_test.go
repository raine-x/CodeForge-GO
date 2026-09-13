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
