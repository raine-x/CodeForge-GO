package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// httpClient 用于流式请求；总超时由 context 控制。
var httpClient = &http.Client{}

// transientStatus 是上游网关常见的瞬时故障状态码（限流、过载、网关抖动、
// 权限校验抖动）。403 虽通常表示密钥无权限，但网关侧偶发未同步 / 抖动
// 也会返回它（实测同请求重试可恢复），故一并纳入自动重试。
var transientStatus = map[int]bool{
	403: true, // Forbidden（网关侧权限抖动 / 未同步）
	408: true, // Request Timeout
	425: true, // Too Early
	429: true, // Too Many Requests
	500: true, // Internal Server Error
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
}

// IsTransientStatus 判断状态码是否属于可自动重试的瞬时故障（供测试连接等复用）。
func IsTransientStatus(code int) bool { return transientStatus[code] }

// RetryHook 在每次自动重试前被调用，用于向前端透出「正在重试」与原因。
// attempt 是即将进行的第几次尝试（1-based）。
type RetryHook func(attempt int, reason string)

type retryHookCtxKey struct{}

// WithRetryHook 将重试回调注入 context。
func WithRetryHook(ctx context.Context, h RetryHook) context.Context {
	return context.WithValue(ctx, retryHookCtxKey{}, h)
}

// retryHookFrom 从 context 取出重试回调（未注入返回 nil）。
func retryHookFrom(ctx context.Context) RetryHook {
	if h, ok := ctx.Value(retryHookCtxKey{}).(RetryHook); ok {
		return h
	}
	return nil
}

const (
	defaultMaxAttempts  = 5
	defaultRetryBackoff = 1500 * time.Millisecond
	maxRetryAfter       = 30 * time.Second
	maxErrorBodyBytes   = 8192
	maxStreamLineBytes  = 8 << 20
	streamReadBufBytes  = 64 * 1024
)

// RetryPolicy 描述上游瞬时故障的重试策略。
type RetryPolicy struct {
	// MaxAttempts 是含首次请求在内的总尝试次数；<=0 时取默认值 5。
	MaxAttempts int
	// Backoff 是首次退避间隔，之后按 2 倍指数增长；<=0 时取默认值 1.5s。
	Backoff time.Duration
}

// normalize 补齐默认值。
func (p RetryPolicy) normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = defaultMaxAttempts
	}
	if p.Backoff <= 0 {
		p.Backoff = defaultRetryBackoff
	}
	return p
}

// postJSON 发送 JSON POST 请求并返回响应流；对上游瞬时故障自动指数退避重试。
//
// 重试仅作用于「首次响应」阶段：一旦开始读取流式响应体便不再重试，
// 以免重复计费或产生重复内容。
func postJSON(
	ctx context.Context,
	url string,
	headers map[string]string,
	payload any,
	policy RetryPolicy,
) (*http.Response, error) {
	policy = policy.normalize()

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	delay := policy.Backoff
	var lastErr error

	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		for k, v := range headers {
			if v != "" {
				req.Header.Set(k, v)
			}
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			// 网络层错误（连接重置 / DNS 抖动等）同样值得重试。
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("LLM 请求失败: %w", err)
			if attempt < policy.MaxAttempts {
				log.Printf("[llm] 请求异常，准备第 %d 次重试: %v", attempt+1, err)
				if hook := retryHookFrom(ctx); hook != nil {
					hook(attempt+1, lastErr.Error())
				}
			}
			continue
		}

		if resp.StatusCode < 400 {
			return resp, nil
		}

		retryAfter := parseRetryAfter(resp)
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		_ = resp.Body.Close()
		apiErr := fmt.Errorf("LLM 请求失败 (%d): %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		lastErr = apiErr

		if !transientStatus[resp.StatusCode] || attempt >= policy.MaxAttempts {
			return nil, apiErr
		}
		if retryAfter > 0 {
			delay = retryAfter
		}
		if hook := retryHookFrom(ctx); hook != nil {
			hook(attempt+1, apiErr.Error())
		}
		log.Printf("[llm] 上游返回 %d，第 %d/%d 次尝试，%.1fs 后重试",
			resp.StatusCode, attempt, policy.MaxAttempts, delay.Seconds())
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("LLM 请求失败：重试 %d 次后仍不可用", policy.MaxAttempts)
	}
	return nil, lastErr
}

// parseRetryAfter 解析 Retry-After 响应头（秒数形式），并做上限保护。
func parseRetryAfter(resp *http.Response) time.Duration {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return 0
	}
	d := time.Duration(secs) * time.Second
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	return d
}

// scanSSE 解析 SSE 流，按事件回调。event 为 event: 字段，data 为拼接后的 data 负载。
func scanSSE(r io.Reader, fn func(event, data string)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, streamReadBufBytes), maxStreamLineBytes)

	var event string
	var data strings.Builder
	flush := func() {
		if data.Len() == 0 && event == "" {
			return
		}
		fn(event, data.String())
		event = ""
		data.Reset()
	}

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, ":"):
			// 注释 / 心跳，忽略
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	flush()
	return scanner.Err()
}

// send 在 context 取消时安全发送事件。
func send(ctx context.Context, out chan<- StreamEvent, ev StreamEvent) {
	select {
	case out <- ev:
	case <-ctx.Done():
	}
}
