package plugins

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"codeforge/config"
	"codeforge/pkg/security"
	"codeforge/pkg/tools"
)

// fakeMCPHTTPServer 模拟 Streamable HTTP MCP 服务端：返回一次性 JSON 响应。
// 记录收到的 method 序列，便于断言握手流程。
func fakeMCPHTTPServer(t *testing.T, methodsSeen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		*methodsSeen = append(*methodsSeen, msg.Method)

		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "回显输入",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
						"required": []string{"text"},
					},
				},
			}}
		case "tools/call":
			var params struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			json.Unmarshal(msg.Params, &params)
			result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo:" + params.Arguments.Text}},
			}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
}

// 端到端：初始化握手 → 工具发现（验证方法顺序）。
func TestMCPHTTPDriverEndToEnd(t *testing.T) {
	var methods []string
	srv := fakeMCPHTTPServer(t, &methods)
	defer srv.Close()

	cfg := config.PluginConfig{Name: "fake", Type: "mcp-http", Endpoint: srv.URL}
	d, err := newMCPHTTPDriver(cfg)
	if err != nil {
		t.Fatalf("构造驱动失败: %v", err)
	}
	defer d.Close()

	tools_, err := d.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	// 握手顺序：initialize → tools/list
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "tools/list" {
		t.Fatalf("握手顺序不对: %v", methods)
	}
	if len(tools_) != 1 || tools_[0].Name() != "echo" {
		t.Fatalf("工具发现不对: %+v", tools_)
	}
}

// 兼容 tools.Tool 的实际返回：直接用 RemoteTool.Execute 验证更真实。
func TestMCPHTTPDriverCallRealTool(t *testing.T) {
	var methods []string
	srv := fakeMCPHTTPServer(t, &methods)
	defer srv.Close()

	cfg := config.PluginConfig{Name: "fake", Type: "mcp-http", Endpoint: srv.URL}
	d, err := newMCPHTTPDriver(cfg)
	if err != nil {
		t.Fatalf("构造驱动失败: %v", err)
	}
	defer d.Close()

	tools_, err := d.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	res, err := tools_[0].Execute(context.Background(), json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("Execute err: %v", err)
	}
	if !res.Success || res.Data != "echo:hi" {
		t.Fatalf("结果不对: %+v", res)
	}
	if len(methods) < 3 || methods[2] != "tools/call" {
		t.Fatalf("应发生 tools/call: %v", methods)
	}
}

// SSE 流响应解析：Server 返回 text/event-stream（Exa 就是这种），驱动应能解析并
// **正确解包 JSON-RPC 信封**。
//
// ⚠️ 这条测试必须断言「真的解析出了工具与调用结果」，不能只断言 err == nil ——
// 2026-09-16 的 bug 就是 SSE 路径少了解包这一步：返回的是整个信封而不是 result，
// 上层按 result 结构解析得到空列表，**全程零报错**。当时这条测试用的正是空 tools 数组
// 且只断言无错，所以完全没拦住。
func TestMCPHTTPDriverSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		w.Header().Set("Content-Type", "text/event-stream")
		var result any
		switch msg.Method {
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "回显输入",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "sse-ok"}}}
		default:
			result = map[string]any{}
		}
		// 整个 JSON-RPC 信封放进 data 行（与真实 SSE 服务器一致）
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		_, _ = w.Write([]byte("event: message\ndata: " + string(payload) + "\n\n"))
	}))
	defer srv.Close()

	cfg := config.PluginConfig{Name: "fake", Type: "mcp-http", Endpoint: srv.URL}
	d, err := newMCPHTTPDriver(cfg)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer d.Close()

	tools_, err := d.Tools(context.Background())
	if err != nil {
		t.Fatalf("SSE 流 tools/list 失败: %v", err)
	}
	// 关键断言：信封必须被解包，工具要真的出现
	if len(tools_) != 1 || tools_[0].Name() != "echo" {
		t.Fatalf("SSE 路径未正确解包 tools/list（期望 1 个 echo），实际 %d 个: %+v", len(tools_), tools_)
	}
	res, err := tools_[0].Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("SSE 流 tools/call 失败: %v", err)
	}
	if !res.Success || res.Data != "sse-ok" {
		t.Fatalf("SSE 路径 tools/call 结果不对（期望 sse-ok）: %+v", res)
	}
}

// 回归：插件管理器加载时会把工具名改成限定名（插件名.工具名）用于本地路由，
// 但发给远端服务器的 tools/call 必须用发现时的原始名 —— 否则远端不认识，
// 返回 "Unknown tool: 插件名.工具名"（2026-09-16 Parallel Search 的真实故障：
// 模型调用名链路全程正确，倒在这最后一步）。
func TestRemoteToolUsesRemoteNameAfterQualify(t *testing.T) {
	var calledName string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == "tools/call" {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			calledName = p.Name
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "fake", "version": "1.0"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "web_search", "description": "搜索", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": result})
	}))
	defer srv.Close()

	d, err := newMCPHTTPDriver(config.PluginConfig{Name: "parallel_search", Type: "mcp-http", Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	defer d.Close()

	tools_, err := d.Tools(context.Background())
	if err != nil {
		t.Fatalf("Tools 失败: %v", err)
	}
	rt, ok := tools_[0].(*RemoteTool)
	if !ok {
		t.Fatalf("期望 *RemoteTool，实际 %T", tools_[0])
	}
	// 模拟 manager.loadLocked 的限定名改写
	rt.name = qualify("parallel_search", rt.name)
	if rt.Name() != "parallel_search.web_search" {
		t.Fatalf("限定名不对: %s", rt.Name())
	}

	if _, err := rt.Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if calledName != "web_search" {
		t.Fatalf("远端应收到的原始名 %q，实际收到 %q（限定名泄露给了远端）", "web_search", calledName)
	}
}

// 联网冒烟（可选）：仓库预置的两个远程 MCP 端点都必须能握手 → 发现工具 → 真实调用。
// 默认跳过 —— 依赖外网与上游可用性的用例不能进常规 `go test ./...`，
// 需显式 CODEFORGE_E2E=1 才跑（与 exposure_ab_test.go 同一开关约定）。
//
// ⚠️ **两个端点都要覆盖，不能只测 Parallel**：Parallel 回一次性 JSON，Exa 回 SSE，
// 两条传输路径是不同代码分支。2026-09-16 的 bug（SSE 路径少了解包 JSON-RPC 信封 →
// 零报错但 0 个工具）就是「只冒烟 Parallel」时漏掉的。
func TestMCPHTTPLiveEndpoints(t *testing.T) {
	if os.Getenv("CODEFORGE_E2E") != "1" {
		t.Skip("联网冒烟需 CODEFORGE_E2E=1")
	}
	cases := []struct {
		name     string
		endpoint string
		tool     string // 期望发现的工具名（用 Exa/Parallel 各自的第一方工具）
		args     string
	}{
		{
			name:     "parallel",
			endpoint: "https://search.parallel.ai/mcp",
			tool:     "web_search",
			args:     `{"objective":"Go 语言 MCP 客户端实现","search_queries":["Go MCP client"]}`,
		},
		{
			name:     "exa",
			endpoint: "https://mcp.exa.ai/mcp",
			tool:     "web_search_exa",
			args:     `{"query":"Go language MCP client implementation","objective":"确认 Exa 的 SSE 响应能被正确解包"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.PluginConfig{Name: c.name, Type: "mcp-http", Endpoint: c.endpoint}
			d, err := newMCPHTTPDriver(cfg)
			if err != nil {
				t.Fatalf("构造失败: %v", err)
			}
			defer d.Close()

			tools_, err := d.Tools(context.Background())
			if err != nil {
				t.Fatalf("握手/发现失败: %v", err)
			}
			names := make([]string, 0, len(tools_))
			for _, tl := range tools_ {
				names = append(names, tl.Name())
			}
			t.Logf("发现工具: %v", names)
			if len(tools_) == 0 {
				t.Fatalf("零工具 —— 疑似信封未解包（零报错但上层拿到空 result）")
			}

			// 按名字取目标工具（别用下标：上游随时可能调整顺序或增删工具）
			var target tools.Tool
			for _, tl := range tools_ {
				if tl.Name() == c.tool {
					target = tl
				}
			}
			if target == nil {
				t.Fatalf("未发现期望的工具 %q，实际: %v", c.tool, names)
			}

			// 真实调用：验证 tools/call 的返回同样被正确解包
			res, err := target.Execute(context.Background(), json.RawMessage(c.args))
			if err != nil {
				t.Fatalf("%s 调用失败: %v", c.tool, err)
			}
			if !res.Success {
				t.Fatalf("%s 返回失败: %+v", c.tool, res)
			}
			text, _ := res.Data.(string)
			t.Logf("%s 返回前 160 字: %s", c.tool, truncateStr(text, 160))
			if len(text) < 40 {
				t.Fatalf("返回内容过短，疑似解析失败: %q", text)
			}
		})
	}
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// 联网冒烟（端到端，走真实调用链）：NewManager.LoadAll 会把工具名限定为
// 「插件名.工具名」，再从注册表按限定名取出来执行 —— 这条路径复刻了
// 2026-09-16 的真实故障现场：限定名若泄露到 tools/call，Parallel 会返回
// "Unknown tool: parallel_search.web_search"，模型便会陷入无效重试。
func TestMCPHTTPLiveManagerQualifiedCall(t *testing.T) {
	if os.Getenv("CODEFORGE_E2E") != "1" {
		t.Skip("联网冒烟需 CODEFORGE_E2E=1")
	}
	registry := tools.NewRegistry()
	policy := security.NewPolicy(config.SecurityConfig{})
	mgr := NewManager(registry, policy, []config.PluginConfig{{
		Name: "parallel_search", Type: "mcp-http", Enabled: true,
		Endpoint: "https://search.parallel.ai/mcp",
	}})
	defer mgr.Stop()

	if err := mgr.LoadAll(context.Background()); err != nil {
		t.Fatalf("LoadAll 失败: %v", err)
	}
	tool, ok := registry.Get("parallel_search.web_search")
	if !ok {
		t.Fatalf("注册表里找不到限定名工具 parallel_search.web_search，实际: %v", registry.Names())
	}
	res, err := tool.Execute(context.Background(), json.RawMessage(
		`{"objective":"Go 语言 MCP 客户端实现","search_queries":["Go MCP client"]}`))
	if err != nil {
		t.Fatalf("限定名路径调用失败: %v", err)
	}
	if !res.Success {
		t.Fatalf("限定名路径调用返回失败（疑似限定名泄露给远端）: %+v", res)
	}
	text, _ := res.Data.(string)
	t.Logf("parallel_search.web_search 返回前 160 字: %s", truncateStr(text, 160))
	if len(text) < 40 {
		t.Fatalf("返回内容过短，疑似解析失败: %q", text)
	}
}
