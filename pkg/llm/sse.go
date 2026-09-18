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

// isQuotaExceeded 判断错误体是否属于「配额耗尽」类永久性错误。
// 这类 429 / 403 是账户余额或额度用尽，重试不会恢复（还会白耗次数和等待），
// 须与「真正限流」（rate_limit）区分开：后者短暂，重试可恢复。
func isQuotaExceeded(body []byte) bool {
	s := strings.ToLower(string(body))
	for _, frag := range []string{"quota_exceeded", "quota exceeded", "insufficient_quota", "insufficient quota"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// RetryHook 在每次自动重试前被调用，用于向前端透出「正在重试」与原因。
// attempt 是即将进行的第几次尝试（1-based），maxAttempts 是含首次请求在内的总尝试次数。
type RetryHook func(attempt, maxAttempts int, reason string)

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
	defaultRetryBackoff = 1000 * time.Millisecond
	maxRetryAfter       = 30 * time.Second
	maxErrorBodyBytes   = 8192
	maxStreamLineBytes  = 8 << 20
	streamReadBufBytes  = 64 * 1024
)

// backoffMultipliers 是常用退避序列：1×→2×→3×→6×（之后封顶 6×）。
// 比纯指数（1→2→4→8）更平滑，前几次快速重试捞回瞬时抖动，后面留足时间
// 让上游限流窗口恢复。
var backoffMultipliers = []int{1, 2, 3, 6}

// RetryPolicy 描述上游瞬时故障的重试策略。
type RetryPolicy struct {
	// MaxAttempts 是含首次请求在内的总尝试次数；<=0 时取默认值 5。
	MaxAttempts int
	// Mode 是间隔模式：fixed（每次等同样时长）或 backoff（1×→2×→3×→6× 序列，
	// 封顶 6×）；空值按 backoff。
	Mode string
	// Backoff 是基础间隔：fixed 模式即每次等待时长；backoff 模式是序列基准
	//（1000ms → 1s/2s/3s/6s）；<=0 时取默认值 1s。
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
	// 只认 fixed / backoff，其余（含大小写脏值）一律按 backoff。
	mode := strings.ToLower(strings.TrimSpace(p.Mode))
	if mode != "fixed" && mode != "backoff" {
		mode = "backoff"
	}
	p.Mode = mode
	return p
}

// attemptDelay 返回第 attempt 次（>=2，即首次重试）请求前的等待时长。
// 上游给了 Retry-After 时由调用方覆盖。
func (p RetryPolicy) attemptDelay(attempt int) time.Duration {
	if p.Mode == "fixed" {
		return p.Backoff
	}
	idx := attempt - 2
	if idx < 0 {
		idx = 0
	}
	if idx >= len(backoffMultipliers) {
		idx = len(backoffMultipliers) - 1
	}
	return p.Backoff * time.Duration(backoffMultipliers[idx])
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

	var lastErr error
	var overrideDelay time.Duration // 上游 Retry-After 指定的下一次等待，优先于本地序列

	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if attempt > 1 {
			// 等待时长：上游给了 Retry-After 就听上游的，否则按模式算
			//（fixed 每次相同；backoff 为 1×→2×→3×→6× 序列）。
			delay := overrideDelay
			if delay <= 0 {
				delay = policy.attemptDelay(attempt)
			}
			overrideDelay = 0
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
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
					hook(attempt+1, policy.MaxAttempts, lastErr.Error())
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

		if !transientStatus[resp.StatusCode] || isQuotaExceeded(msg) || attempt >= policy.MaxAttempts {
			return nil, apiErr
		}
		nextDelay := policy.attemptDelay(attempt + 1)
		if retryAfter > 0 {
			overrideDelay = retryAfter
			nextDelay = retryAfter
		}
		if hook := retryHookFrom(ctx); hook != nil {
			hook(attempt+1, policy.MaxAttempts, apiErr.Error())
		}
		log.Printf("[llm] 上游返回 %d，第 %d/%d 次尝试，%.1fs 后重试",
			resp.StatusCode, attempt, policy.MaxAttempts, nextDelay.Seconds())
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
