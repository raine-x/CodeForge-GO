package errs

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// StatusError 是「带 HTTP 状态码的上游错误」。
//
// 为什么必须有这个类型：上游状态码此前只以字符串形式留在
// `fmt.Errorf("LLM 请求失败 (%d): %s", code, body)` 里，Classify 拿不到，
// 只能靠猜错误文本（"rate limit" / "unauthorized"）。
//
// 而实测 GLM 的 429 body 是
// `{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}` ——
// 一个限流关键词都没有，掉进 KindUnknown。401 同理。
//
// 放在 errs 而不是 llm 的理由：llm 已经 import errs（openai.go:13），
// 反过来会成循环依赖。
type StatusError struct {
	Code int
	// Body 是上游原始响应体（已截断），只用于分类兜底与日志，不进 Error()。
	Body string
	// RetryAfter 是上游 Retry-After 头（0 = 未给）。
	// 保留它是为了让上层能说清「上游让我等 N 秒」而不是干等。
	RetryAfter time.Duration
	// Msg 是给用户看的一句话（含状态码与上游 body）。
	Msg string

	err error
}

func (e *StatusError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Msg
}

// Unwrap 支持任意层 %w 包装。
//
// 这一条不是锦上添花：pkg/llm/sse.go 有一处
// `fmt.Errorf("%w（本次送出：%s）", apiErr, ...)`，用 %s 断链的话
// 状态码就取不到了，分类会静默失效。
func (e *StatusError) Unwrap() error { return e.err }

// NewStatus 构造带状态码的上游错误。
//
// ⚠️ msg 的格式 `LLM 请求失败 (%d): %s` 是**前端契约**：
// web/dist/ui.js 的 describeLLMError 与 retryReasonBrief 都用
// /\((\d{3})\)/ 从错误串里抠状态码，改它会同时打破那 12 条前端断言。
// 有测试钉住（status_test.go 的 TestStatusErrorMessageFormatKeepsParensCode）。
func NewStatus(code int, body string, retryAfter time.Duration, cause error) *StatusError {
	return &StatusError{
		Code:       code,
		Body:       body,
		RetryAfter: retryAfter,
		Msg:        fmt.Sprintf("LLM 请求失败 (%d): %s", code, strings.TrimSpace(body)),
		err:        cause,
	}
}

// StatusCode 从错误链里取出 HTTP 状态码；取不到返回 0。
//
// 沿 Unwrap 一路下钻，是「各种包装形式」的唯一可靠解法。
func StatusCode(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

// StatusRetryAfter 从错误链里取出上游要求的等待时长；没有返回 0。
func StatusRetryAfter(err error) time.Duration {
	var se *StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}
