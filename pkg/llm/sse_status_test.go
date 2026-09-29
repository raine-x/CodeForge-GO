package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codeforge/pkg/errs"
)

// 状态码从 LLM 层流到分类器。
//
// 改造前 pkg/llm/sse.go 用 `fmt.Errorf("LLM 请求失败 (%d): %s", ...)`
// 把状态码拍平成字符串，errs.Classify 拿不到，只能猜 body 文本。
// 而实测 GLM 的 429 body 里一个限流关键词都没有。

func testPolicy(max int) RetryPolicy {
	return RetryPolicy{MaxAttempts: max, Backoff: 5 * time.Millisecond}
}

// TestPostJSONErrorCarriesStatusCode 抓主线：错误必须带得住状态码。
func TestPostJSONErrorCarriesStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}`))
	}))
	defer srv.Close()

	// MaxAttempts=1：第一次就返回，既能拿到带字段的错误，又不必真等那 7 秒。
	// 「429 会不会退避重试」由 TestPostJSONStillRetriesRateLimit429 单独验。
	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(1))
	if err == nil {
		t.Fatal("429 应报错")
	}
	if got := errs.StatusCode(err); got != 429 {
		t.Fatalf("错误应带状态码 429，实际 %d（错误串 %q）", got, err.Error())
	}
	if !errs.Retryable(errs.Classify(err)) {
		t.Errorf("429 应可重试，实际分类 %v", errs.Classify(err))
	}
	// Retry-After 必须随错误走上去 —— 上层要能说清「上游让你等 7 秒」而不是干等
	if got := errs.StatusRetryAfter(err); got != 7*time.Second {
		t.Errorf("Retry-After 应随错误带上来（7s），实际 %v", got)
	}
}

// TestPostJSONDoesNotRetryForbidden 403 立即上报。
//
// 403 此前在 transientStatus 里（注释说「实测同请求重试可恢复」），
// 于是密钥无权限也会被重试 5 次 —— 白等 1+2+3+6+6=18 秒，
// 还把「该去改密钥」这个提示推迟到 18 秒之后。
//
// 另一层考虑：tests/llm_client.py 的 TRANSIENT_STATUS 里**本来就没有 403**，
// Go 与 Python 两侧不一致，说明这条没被 e2e 覆盖。
func TestPostJSONDoesNotRetryForbidden(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient permissions"}}`))
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(5))
	if err == nil {
		t.Fatal("403 应直接失败")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("403 不应重试，实际请求 %d 次", got)
	}
	if got := errs.StatusCode(err); got != 403 {
		t.Errorf("错误应带状态码 403，实际 %d", got)
	}
	if errs.Retryable(errs.Classify(err)) {
		t.Error("403 不该被判为可重试")
	}
}

// TestPostJSONDoesNotRetryUnauthorized 401 同理：一步到位。
func TestPostJSONDoesNotRetryUnauthorized(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		// 空 body：字符串兜底完全无从下手
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(5))
	if err == nil {
		t.Fatal("401 应直接失败")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("401 不应重试，实际请求 %d 次", got)
	}
	if errs.Classify(err) != errs.KindAuth {
		t.Errorf("401 应归为鉴权失败，实际 %v", errs.Classify(err))
	}
}

// TestPostJSONStillRetriesServerErrors 回归：5xx 仍要退避重试。
// 改判定依据（transientStatus → errs）之后这条必须还是绿的。
func TestPostJSONStillRetriesServerErrors(t *testing.T) {
	for _, code := range []int{500, 502, 503, 504} {
		var calls int32
		c := code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&calls, 1)
			w.WriteHeader(c)
		}))
		_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(3))
		srv.Close()
		if err == nil {
			t.Errorf("%d 应最终报错", c)
		}
		if got := atomic.LoadInt32(&calls); got != 3 {
			t.Errorf("%d 应重试满 3 次，实际 %d 次", c, got)
		}
	}
}

// TestPostJSONStillRetriesRateLimit429 回归：429 仍要退避重试。
func TestPostJSONStillRetriesRateLimit429(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}`))
	}))
	defer srv.Close()

	_, _ = postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(3))
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("429 应重试满 3 次，实际 %d 次", got)
	}
}

// TestPostJSONStillDoesNotRetryQuota 回归：配额耗尽不重试。
// 这是 429 内部的第二类，isQuotaExceeded 继续负责，不能被新判定吃掉。
func TestPostJSONStillDoesNotRetryQuota(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"insufficient_quota","message":"quota exceeded"}}`))
	}))
	defer srv.Close()

	_, _ = postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(5))
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("配额耗尽不该重试，实际 %d 次", got)
	}
}

// TestStatusErrorKeepsFrontendFormat 前端 ui.js 用 /\((\d{3})\)/ 抠状态码。
func TestStatusErrorKeepsFrontendFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`busy`))
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.URL, nil, map[string]any{}, testPolicy(1))
	if err == nil {
		t.Fatal("应报错")
	}
	if !strings.Contains(err.Error(), "(429)") {
		t.Errorf("错误串必须保留 (429) 形式（ui.js 的 describeLLMError 依赖它）: %q", err.Error())
	}
}
