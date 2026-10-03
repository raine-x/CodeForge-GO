package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 说明：上游免费/共享网关会偶发 429 / 5xx（如 "gateway overloaded"），
// 这些瞬时故障必须自动重试，而 4xx 客户端错误不得重试。

func TestPostJSONRetriesTransientStatus(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"gateway overloaded"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	policy := RetryPolicy{MaxAttempts: 5, Backoff: 5 * time.Millisecond}
	resp, err := postJSON(context.Background(), srv.URL, nil, map[string]any{"a": 1}, policy, nil, 0)
	if err != nil {
		t.Fatalf("期望重试后成功，实际报错: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("期望请求 3 次（2 次失败 + 1 次成功），实际 %d 次", got)
	}
}

func TestPostJSONDoesNotRetryClientError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	policy := RetryPolicy{MaxAttempts: 5, Backoff: 5 * time.Millisecond}
	if _, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, policy, nil, 0); err == nil {
		t.Fatal("401 应直接失败，不应重试")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("401 不应重试，实际请求 %d 次", got)
	}
}

func TestPostJSONExhaustsRetries(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"cache_only_cold"}}`))
	}))
	defer srv.Close()

	policy := RetryPolicy{MaxAttempts: 3, Backoff: 5 * time.Millisecond}
	if _, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, policy, nil, 0); err == nil {
		t.Fatal("持续 503 应最终返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("期望尝试 3 次后放弃，实际 %d 次", got)
	}
}

func TestPostJSONRespectsContextCancellation(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	policy := RetryPolicy{MaxAttempts: 20, Backoff: 200 * time.Millisecond}
	if _, err := postJSON(ctx, srv.URL, nil, map[string]any{}, policy, nil, 0); err == nil {
		t.Fatal("context 超时后应返回错误")
	}
	if got := atomic.LoadInt32(&calls); got > 5 {
		t.Errorf("context 取消后不应继续重试，实际请求 %d 次", got)
	}
}

func TestRetryPolicyNormalize(t *testing.T) {
	p := RetryPolicy{}.normalize()
	if p.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts 默认值期望 %d，实际 %d", defaultMaxAttempts, p.MaxAttempts)
	}
	if p.Backoff != defaultRetryBackoff {
		t.Errorf("Backoff 默认值期望 %v，实际 %v", defaultRetryBackoff, p.Backoff)
	}

	custom := RetryPolicy{MaxAttempts: 2, Backoff: time.Second}.normalize()
	if custom.MaxAttempts != 2 || custom.Backoff != time.Second {
		t.Errorf("显式配置不应被覆盖: %+v", custom)
	}
}

// 429 但也带 quota_exceeded（余额/配额耗尽）时不应重试。
func TestPostJSONDoesNotRetryQuotaExceeded(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded","type":"quota_exceeded","code":"quota_exceeded"}}`))
	}))
	defer srv.Close()

	policy := RetryPolicy{MaxAttempts: 5, Backoff: 5 * time.Millisecond}
	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, policy, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("quota_exceeded 应直接报错，实际 %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("quota_exceeded 不应重试，实际请求 %d 次", got)
	}
}

// 429 真正限流（rate_limit）仍应重试并在成功后返回。
func TestPostJSONRetriesRateLimit429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	policy := RetryPolicy{MaxAttempts: 5, Backoff: 5 * time.Millisecond}
	resp, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, policy, nil, 0)
	if err != nil {
		t.Fatalf("rate limit 重试后应成功，实际 %v", err)
	}
	defer resp.Body.Close()
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("rate limit 应重试到成功，实际请求 %d 次", got)
	}
}
