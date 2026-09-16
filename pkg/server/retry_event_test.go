package server

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
)

// 上游持续 503 的假 LLM 网关：每次请求都返回瞬时故障，逼出自动重试。
// 返回体里带一句人话，用于断言前端能拿到可读原因。
func fakeFlakyLLM(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"1305","message":"该模型当前访问量过大，请您稍后再试"}}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// 上游瞬时故障重试必须把「第几次 / 共几次 / 原因」透给前端。
//
// 只透一句笼统的「请求失败，正在重试…」时，用户分不清偶发抖动与上游持续故障
// （实测 429 会连撞满 5 次）；本用例钉住这几个字段真的走到了 WS 事件里。
// 全程不依赖外网：假网关固定 503。
func TestRetryEventCarriesAttemptAndReason(t *testing.T) {
	dir := t.TempDir()

	var hits int
	gateway := fakeFlakyLLM(t, &hits)

	cfg := config.Default()
	cfg.Agent.WorkDir = dir
	cfg.Agent.MaxSteps = 2
	cfg.DataDir = filepath.Join(dir, ".codeforge")
	cfg.AuditLog = filepath.Join(dir, ".codeforge", "audit.jsonl")
	cfg.LLM = config.LLMConfig{
		Provider:       "custom",
		BaseURL:        gateway.URL,
		APIKey:         "test-key",
		Model:          "flaky",
		MaxTokens:      256,
		Temperature:    0,
		MaxAttempts:    3, // 缩短重试链，测试才跑得快
		RetryBackoffMs: 10,
	}

	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		t.Fatalf("构造 LLM 适配器失败: %v", err)
	}

	registry := tools.NewRegistry()
	fsys := builtin.NewFS(dir)
	builtin.RegisterFS(registry, fsys)

	policy := security.NewPolicy(cfg.Security)
	audit, err := security.NewAuditLogger(cfg.AuditLog)
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })

	executor := tools.NewExecutor(registry, policy, audit, nil, 30*time.Second, 32*1024)
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	history := agent.NewHistory(st)
	if _, err := history.Create(dir, ""); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	ag := agent.New(cfg.Agent, cfg.LLM, provider, executor, history, dir)

	ts := httptest.NewServer(New(cfg, ag, executor, registry, fsys).Routes())
	defer ts.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取鉴权 Cookie 失败: %v", err)
	}
	_ = resp.Body.Close()

	dialer := websocket.Dialer{Jar: jar}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("WebSocket 握手失败: %v", err)
	}
	defer conn.Close()

	var ready map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("读取 ready 帧失败: %v", err)
	}

	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": "你好"}); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}

	var retries []map[string]any
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		if ev["type"] == "retry" {
			retries = append(retries, ev)
		}
		if ev["type"] == "idle" {
			break
		}
	}

	if len(retries) == 0 {
		t.Fatalf("上游持续 503 却没有任何 retry 事件（hits=%d）", hits)
	}
	t.Logf("收到 %d 个 retry 事件，网关被请求 %d 次", len(retries), hits)

	for i, ev := range retries {
		attempt, okA := ev["attempt"].(float64)
		maxAttempts, okM := ev["max_attempts"].(float64)
		if !okA || attempt < 2 {
			t.Errorf("第 %d 个 retry 事件缺少可用的 attempt（实际 %v）—— 前端无法显示「第几次」", i+1, ev["attempt"])
		}
		if !okM || int(maxAttempts) != cfg.LLM.MaxAttempts {
			t.Errorf("第 %d 个 retry 事件的 max_attempts 期望 %d，实际 %v", i+1, cfg.LLM.MaxAttempts, ev["max_attempts"])
		}
		reason, _ := ev["error"].(string)
		if reason == "" {
			t.Errorf("第 %d 个 retry 事件没有 error 原因", i+1)
		}
		// 原因要够「具体」：必须含状态码，前端才能分类成「上游过载」并带出上游原话
		if !strings.Contains(reason, "503") {
			t.Errorf("第 %d 个 retry 事件的原因不含状态码 503，前端无法分类：%q", i+1, reason)
		}
		if !strings.Contains(reason, "该模型当前访问量过大") {
			t.Errorf("第 %d 个 retry 事件的原因没带出上游原话：%q", i+1, reason)
		}
	}

	// attempt 必须递增（前端靠它显示「第 2/3 次 → 第 3/3 次」）
	for i := 1; i < len(retries); i++ {
		prev, _ := retries[i-1]["attempt"].(float64)
		cur, _ := retries[i]["attempt"].(float64)
		if cur <= prev {
			t.Errorf("attempt 应递增：第 %d 个=%v，第 %d 个=%v", i, prev, i+1, cur)
		}
	}
}
