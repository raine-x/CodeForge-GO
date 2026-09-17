package llm

import (
	"strings"
	"testing"
)

// 异常结束必须「一次错误、零正常 stop」，否则上层会把截断当正常完成。
func assertAborted(t *testing.T, evs []StreamEvent, wantErr string) {
	t.Helper()
	var errs, stops int
	var lastErr string
	for _, ev := range evs {
		if ev.Type == EventError {
			errs++
			lastErr = ev.Error
		}
		if ev.Type == EventMessageStop {
			stops++
		}
	}
	if errs != 1 {
		t.Fatalf("异常结束应恰好一次错误，实际 %d 次（last=%q）", errs, lastErr)
	}
	if stops != 0 {
		t.Fatalf("异常结束不应发送正常 MessageStop，实际 %d 次", stops)
	}
	if !strings.Contains(lastErr, wantErr) {
		t.Fatalf("错误应包含 %q，实际 %q", wantErr, lastErr)
	}
}

// OpenAI content_filter：上游命中内容过滤而截断，必须报错，不能当正常完成。
func TestOpenAIConsumeContentFilterAborts(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"部分文本"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	assertAborted(t, runConsume(t, body), "内容过滤")
}

// OpenAI length 截断在报错后不得再发正常 MessageStop（旧实现会先报错再发 stop）。
func TestOpenAIConsumeLengthNoStopAfterError(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"思考到一半"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	assertAborted(t, runConsume(t, body), "max_tokens")
}

// Anthropic max_tokens 截断：message_delta.stop_reason=max_tokens，必须报错。
func TestAnthropicConsumeMaxTokensAborts(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"回答到一半"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}`,
		``,
	}, "\n")
	assertAborted(t, runAnthropicConsume(t, body), "max_tokens")
}

// Anthropic 流被掐断（无 message_delta / 无 stop_reason）不能伪装成正常结束。
func TestAnthropicConsumeEarlyEOFAborts(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"部分"}}`,
		``,
	}, "\n")
	assertAborted(t, runAnthropicConsume(t, body), "提前结束")
}

// Anthropic 正常结束（end_turn）不受影响：无错误、恰一次 stop。
func TestAnthropicConsumeNormalEndStillStops(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`,
		``,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"你好"}}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var errs, stops int
	for _, ev := range runAnthropicConsume(t, body) {
		if ev.Type == EventError {
			errs++
		}
		if ev.Type == EventMessageStop {
			stops++
		}
	}
	if errs != 0 || stops != 1 {
		t.Fatalf("正常结束 errors=%d stops=%d", errs, stops)
	}
}
