package server

import (
	"context"
	"encoding/json"
	"fmt"
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

// stubProvider 是用于测试的假 LLM 适配器：固定回一段文本后结束。
type stubProvider struct{}

func (stubProvider) Name() string { return "stub" }

func (stubProvider) Stream(_ context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 4)
	go func() {
		defer close(ch)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "你好，"}
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "我是 CodeForge。"}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// testDeps 汇总一个可用的服务依赖集合。
type testDeps struct {
	cfg      *config.Config
	agent    *agent.Agent
	executor *tools.Executor
	registry *tools.Registry
	fsys     *builtin.FS
	dir      string
}

// newTestDeps 在临时目录中组装完整的服务依赖。
func newTestDeps(t *testing.T) *testDeps { return newTestDepsAt(t, "") }

// newTestDepsAt 与 newTestDeps 相同，但可指定一个真实的配置目录。
//
// configDir 非空时 cfg 会绑定该目录（ConfigDir() != ""），于是
// 「写回 config/local.yaml」「模型库落盘 config/models.yaml」这些
// 依赖配置目录的行为才真正被覆盖到；为空则等同 config.Default()，
// 模型库退化为纯内存库（见 modelStorePath）。
func newTestDepsAt(t *testing.T, configDir string) *testDeps {
	return newTestDepsAtProvider(t, configDir, stubProvider{})
}

// newTestDepsAtProvider 与 newTestDepsAt 相同，但可替换 LLM 适配器
// （用于断言 WS 参数是否真的进入请求，例如思考强度）。
func newTestDepsAtProvider(t *testing.T, configDir string, provider llm.Provider) *testDeps {
	t.Helper()
	dir := t.TempDir()

	cfg := config.Default()
	if configDir != "" {
		loaded, err := config.Load(configDir)
		if err != nil {
			t.Fatalf("加载测试配置失败: %v", err)
		}
		cfg = loaded
	}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.AutoOpen = false
	cfg.Agent.WorkDir = dir
	cfg.DataDir = filepath.Join(dir, ".codeforge")
	cfg.AuditLog = filepath.Join(dir, ".codeforge", "audit.jsonl")

	registry := tools.NewRegistry()
	fsys := builtin.NewFS(dir)
	builtin.RegisterFS(registry, fsys)
	builtin.RegisterTerminal(registry, fsys)
	builtin.RegisterSearch(registry, fsys)

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
	// 与生产一致（main.go）：记忆/检查点与 History 共用同一个 SQLite 库。
	// 不注入的话检查点无处落库，回滚类接口会以「存储未就绪」失败。
	ag.SetMemoryStore(st)

	return &testDeps{
		cfg:      cfg,
		agent:    ag,
		executor: executor,
		registry: registry,
		fsys:     fsys,
		dir:      dir,
	}
}

// newServer 基于依赖构造 Server 实例。
func (d *testDeps) newServer() *Server {
	return New(d.cfg, d.agent, d.executor, d.registry, d.fsys)
}

// newTestServer 启动一个 httptest 服务。
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(newTestDeps(t).newServer().Routes())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthEndpoint(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("健康检查期望 200，实际 %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status 期望 ok，实际 %v", body["status"])
	}
}

func TestAPIRequiresCookie(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 Cookie 访问 /api/config 期望 401，实际 %d", resp.StatusCode)
	}
}

func TestIndexIssuesHttpOnlyCookie(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("首页期望 200，实际 %d", resp.StatusCode)
	}
	var tokenCookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == TokenCookie {
			tokenCookie = c
		}
	}
	if tokenCookie == nil {
		t.Fatal("首页未下发鉴权 Cookie")
	}
	if !tokenCookie.HttpOnly {
		t.Error("鉴权 Cookie 必须为 HttpOnly")
	}
}

// TestWebSocketAgentFlow 端到端验证：Cookie 鉴权 → WS 握手 → 事件流 → 会话持久化。
func TestWebSocketAgentFlow(t *testing.T) {
	ts := newTestServer(t)

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取 Cookie 失败: %v", err)
	}
	_ = resp.Body.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.Dialer{Jar: jar}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 握手失败: %v", err)
	}
	defer conn.Close()

	// 1) 首帧应为 ready
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var first map[string]any
	if err := conn.ReadJSON(&first); err != nil {
		t.Fatalf("读取首帧失败: %v", err)
	}
	if first["type"] != "ready" {
		t.Fatalf("首帧期望 ready，实际 %v", first["type"])
	}

	// 2) 发送用户消息，收集事件
	if err := conn.WriteJSON(map[string]any{
		"type": "user_message",
		"text": "你好",
	}); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}

	var gotText strings.Builder
	sawDone := false
	for i := 0; i < 30 && !sawDone; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		switch ev["type"] {
		case "text":
			gotText.WriteString(asString(ev["text"]))
		case "error":
			t.Fatalf("Agent 返回错误: %v", ev["error"])
		case "done":
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("未收到 done 事件")
	}
	if gotText.String() != "你好，我是 CodeForge。" {
		t.Errorf("流式文本拼接不正确，实际: %q", gotText.String())
	}
}

func TestWebSocketIncompleteUpstream(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, text := range []string{"我直接", "列目录找"} {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": text}}}})
			t.Logf("SSE chunk: %s", chunk)
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			w.(http.Flusher).Flush()
		}
		t.Log("SSE EOF without finish_reason or [DONE]")
	}))
	defer gateway.Close()
	defer gateway.CloseClientConnections()

	deps := newTestDepsAtProvider(t, "", llm.NewOpenAI(config.LLMConfig{
		BaseURL: gateway.URL, Model: "stalled-test", MaxTokens: 256,
	}))
	ts := httptest.NewServer(deps.newServer().Routes())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: time.Second}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	dialer := websocket.Dialer{Jar: jar, HandshakeTimeout: time.Second}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	var ready map[string]any
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": "继续"}); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for {
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("upstream stalled after %q without a terminal error: %v", text.String(), err)
		}
		t.Logf("WS type=%v text=%v error=%v", ev["type"], ev["text"], ev["error"])
		switch ev["type"] {
		case "text":
			text.WriteString(asString(ev["text"]))
		case "done", "idle":
			t.Fatalf("incomplete upstream reported success: %v", ev)
		case "error":
			if text.String() != "我直接列目录找" || asString(ev["error"]) == "" {
				t.Fatalf("missing partial text or explicit error: text=%q event=%v", text.String(), ev)
			}
			return
		}
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
