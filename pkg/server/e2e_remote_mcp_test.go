package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
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
	"codeforge/pkg/tools/plugins"
)

// e2eRemoteMarker 是假远程 MCP 返回内容里的唯一标记，用于证明「结果真的从 HTTP 回来了」。
const e2eRemoteMarker = "REMOTE-MCP-MARKER-777"

// fakeRemoteMCP 是一个最小的远程 MCP（Streamable HTTP）服务：单次 POST 返回一次性 JSON。
// web_search 的结果里带唯一标记，便于断言整条链路真的走通。
func fakeRemoteMCP(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		*calls = append(*calls, msg.Method)

		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-remote-mcp", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "web_search",
				"description": "联网搜索并返回压缩摘录",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"objective":      map[string]any{"type": "string", "description": "搜索目标"},
						"search_queries": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
					"required": []string{"objective"},
				},
			}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": `{"results":[{"url":"https://example.com/a","excerpt":"` + e2eRemoteMarker + `：远程 MCP 驱动通过单次 HTTP POST 完成 tools/call。"}]}`,
			}}}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestE2ERemoteMCPLive 用真实 LLM + 本地假远程 MCP 跑通整条链路：
//
//	模型看到插件工具定义（demo_search.web_search）→ 决定调用 → mcp-http 驱动发 HTTP
//	→ 假服务返回带标记的摘录 → 结果回填 → 模型在回复里复述标记
//
// 假服务在本地，所以不依赖 Parallel.ai 的可用性；但仍需真实 LLM 端点来驱动工具选择。
// 默认跳过；设置环境变量后运行：
//
//	CODEFORGE_E2E=1 LLM_API_KEY=sk-xxx go test ./pkg/server/ -run TestE2ERemoteMCPLive -v -timeout 600s
func TestE2ERemoteMCPLive(t *testing.T) {
	if os.Getenv("CODEFORGE_E2E") == "" {
		t.Skip("未设置 CODEFORGE_E2E=1，跳过真实 LLM 端到端测试")
	}
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if apiKey == "" {
		t.Skip("未设置 LLM_API_KEY，跳过真实 LLM 端到端测试")
	}
	baseURL := envOr("LLM_BASE_URL", "https://api.tokenrouter.com/v1")
	model := envOr("LLM_MODEL", "z-ai/glm-5.3-free")

	dir := t.TempDir()

	// ---- 假远程 MCP 服务
	var mcpCalls []string
	mcp := fakeRemoteMCP(t, &mcpCalls)

	// ---- 服务依赖：插件配置指向假端点
	cfg := config.Default()
	cfg.Agent.WorkDir = dir
	cfg.Agent.MaxSteps = 6
	cfg.DataDir = filepath.Join(dir, ".codeforge")
	cfg.AuditLog = filepath.Join(dir, ".codeforge", "audit.jsonl")
	cfg.LLM = config.LLMConfig{
		Provider:    "custom",
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		MaxTokens:   2048,
		Temperature: 0.2,
	}
	cfg.Plugins = []config.PluginConfig{{
		Name:        "demo_search",
		Type:        "mcp-http",
		Enabled:     true,
		Description: "演示用远程 MCP 搜索服务",
		Endpoint:    mcp.URL,
	}}

	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		t.Fatalf("构造 LLM 适配器失败: %v", err)
	}

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

	executor := tools.NewExecutor(registry, policy, audit, nil, 180*time.Second, 64*1024)
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

	// ---- 插件管理器：加载远程 MCP（与 main.go 同一装配方式）
	manager := plugins.NewManager(registry, policy, cfg.Plugins)
	if err := manager.LoadAll(context.Background()); err != nil {
		t.Fatalf("加载远程 MCP 插件失败: %v", err)
	}
	t.Cleanup(manager.Stop)
	if loaded := manager.Loaded(); len(loaded) != 1 || loaded[0] != "demo_search" {
		t.Fatalf("插件应已加载 demo_search，实际 %v", loaded)
	}

	srv := New(cfg, ag, executor, registry, fsys)
	srv.SetPluginManager(manager)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// ---- WS 连接（Cookie 鉴权）
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
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("读取 ready 帧失败: %v", err)
	}
	if ready["type"] != "ready" {
		t.Fatalf("首帧期望 ready，实际 %v", ready["type"])
	}

	// 远程 MCP 工具必须已进入下发给模型的工具清单（名字带插件前缀）。
	// ready 帧的 tools 是 registry.Names()（字符串数组）。
	readyTools, _ := ready["tools"].([]any)
	var hasRemoteTool bool
	for _, rt := range readyTools {
		if name, ok := rt.(string); ok && name == "demo_search.web_search" {
			hasRemoteTool = true
		}
	}
	if !hasRemoteTool {
		t.Fatalf("ready 帧的工具清单里没有 demo_search.web_search（模型将无法调用远程 MCP），实际: %v", readyTools)
	}
	t.Logf("已连接：工具数=%d，含 demo_search.web_search", len(readyTools))

	// ---- 发一条必须联网的请求
	prompt := "请使用联网搜索能力，搜索「CodeForge 远程 MCP 驱动实现」，" +
		"并把搜索结果原文里的内容（尤其是其中的标记）原样告诉我。"
	if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": prompt}); err != nil {
		t.Fatalf("发送消息失败: %v", err)
	}

	var events []map[string]any
	deadline := time.Now().Add(240 * time.Second)
loop:
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(time.Now().Add(240 * time.Second))
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败: %v", err)
		}
		events = append(events, ev)
		switch ev["type"] {
		case "hitl_request":
			id, _ := ev["approval_id"].(string)
			t.Logf("  ↳ HITL 审批：tool=%v action=%v", ev["tool"], ev["action"])
			if err := conn.WriteJSON(map[string]any{
				"type": "hitl_decision", "approval_id": id, "approved": true,
			}); err != nil {
				t.Fatalf("发送审批决策失败: %v", err)
			}
		case "error":
			t.Fatalf("Agent 返回错误: %v", ev["error"])
		case "idle":
			break loop
		}
	}

	calls := filterType(events, "tool_call")
	results := filterType(events, "tool_result")
	reply := collectText(events)
	t.Logf("  事件类型统计: %s", typeHistogram(events))
	t.Logf("  工具调用: %v", toolNames(calls))
	t.Logf("  MCP 服务收到的 JSON-RPC 方法: %v", mcpCalls)
	t.Logf("  最终回复: %s", truncate(reply, 300))

	// 1) 模型必须调用到远程 MCP 工具（而不是内置 web_fetch 或凭空编造）
	var calledRemote bool
	for _, n := range toolNames(calls) {
		if n == "demo_search.web_search" {
			calledRemote = true
		}
	}
	if !calledRemote {
		t.Fatalf("模型未调用 demo_search.web_search，实际调用: %v", toolNames(calls))
	}

	// 2) HTTP 层真的发生过 tools/call（驱动没走本地短路）
	var sawCall bool
	for _, m := range mcpCalls {
		if m == "tools/call" {
			sawCall = true
		}
	}
	if !sawCall {
		t.Fatalf("假 MCP 服务未收到 tools/call，实际方法序列: %v", mcpCalls)
	}

	// 3) 结果与回复里必须出现标记 —— 证明内容是从 HTTP 响应回填的，不是模型编的
	if !resultsContain(results, e2eRemoteMarker) {
		t.Errorf("工具结果里没有标记 %s，内容未正确回填", e2eRemoteMarker)
	}
	if !strings.Contains(reply, e2eRemoteMarker) {
		t.Errorf("最终回复里没有标记 %s，模型未使用工具结果。回复：%s", e2eRemoteMarker, truncate(reply, 300))
	}

	// 4) 用户可见文本里不得出现工具标识 —— 与 System Prompt 的披露纪律同一条要求。
	//    实测模型会在计划/步骤说明里复述工具名（"我该调用 xxx"），这条断言把它钉住。
	assertNoToolIDInText(t, reply, registry)
}

// assertNoToolIDInText 断言用户可见文本里没有出现任何已注册工具名（含插件限定名及其本名片段）。
//
// 只比对「注册表里真实存在的名字」，不用泛化的 snake_case 正则 —— 后者会把文件名、
// 上游返回的字段名等正常内容误判成泄露。
func assertNoToolIDInText(t *testing.T, text string, registry *tools.Registry) {
	t.Helper()
	for _, name := range registry.Names() {
		if strings.Contains(text, name) {
			t.Errorf("用户可见文本里出现了工具标识 %q（应只用中文功能名）：%s", name, truncate(text, 200))
		}
		// 插件工具是「插件名.工具名」，本名片段单独出现同样是泄露
		if i := strings.Index(name, "."); i > 0 {
			if base := name[i+1:]; base != "" && strings.Contains(text, base) {
				t.Errorf("用户可见文本里出现了工具名片段 %q（来自 %q）：%s", base, name, truncate(text, 200))
			}
		}
	}
}
