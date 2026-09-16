package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/security"
	"codeforge/pkg/tools/plugins"
)

// newTestPluginsServer 组装一个注入了插件管理器的服务（生产链路由 main.go 注入，
// 测试里必须自己接上，否则 /api/plugins 的热加载路径会 nil panic）。
func newTestPluginsServer(t *testing.T, deps *testDeps) *httptest.Server {
	t.Helper()
	srv := deps.newServer()
	srv.SetPluginManager(plugins.NewManager(deps.registry, security.NewPolicy(deps.cfg.Security), deps.cfg.Plugins))
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

// fakeMCPHTTP 起一个最小的远程 MCP（Streamable HTTP）服务：
// initialize / tools/list / tools/call 三段，单次 POST 返回一次性 JSON。
func fakeMCPHTTP(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2025-03-26",
				"serverInfo":      map[string]any{"name": "fake-mcp", "version": "1.0.0"},
			}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "web_search",
				"description": "搜索并返回压缩摘录",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// pluginsListItem 只保留断言关心的字段。
type pluginsListItem struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Endpoint    string `json:"endpoint"`
	Enabled     bool   `json:"enabled"`
	Configured  bool   `json:"configured"`
	Description string `json:"description"`
}

func getPlugins(t *testing.T, client *http.Client, base string) []pluginsListItem {
	t.Helper()
	resp, err := client.Get(base + "/api/plugins")
	if err != nil {
		t.Fatalf("GET /api/plugins 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/plugins 期望 200，实际 %d", resp.StatusCode)
	}
	var body struct {
		Items []pluginsListItem `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析插件列表失败: %v", err)
	}
	return body.Items
}

// 远程 MCP 全链路：POST（type=mcp-http + endpoint）→ 热加载 → 工具以「插件名.工具名」注册
// → GET 回传 endpoint（前端据它展示远程端点）→ 落盘 plugins.yaml。
func TestPluginsAddRemoteMCPHTTP(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	ts := newTestPluginsServer(t, deps)
	client := withAuthClient(t, ts)
	mcp := fakeMCPHTTP(t)

	resp := postJSON(t, client, ts.URL+"/api/plugins", map[string]any{
		"name":        "remote_test",
		"type":        "mcp-http",
		"endpoint":    mcp.URL,
		"description": "远程测试服务",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("添加远程 MCP 期望 200，实际 %d", resp.StatusCode)
	}
	var added map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatalf("解析添加响应失败: %v", err)
	}
	if added["ok"] != true {
		t.Fatalf("添加未成功: %v", added)
	}
	if w, _ := added["warning"].(string); w != "" {
		t.Fatalf("热加载应成功，实际告警: %s", w)
	}

	// 工具必须已注册，且带插件前缀（qualify），否则模型无法调用。
	var toolFound bool
	for _, n := range deps.registry.Names() {
		if n == "remote_test.web_search" {
			toolFound = true
		}
	}
	if !toolFound {
		t.Errorf("远程 MCP 工具未注册，实际注册表: %v", deps.registry.Names())
	}

	// GET 必须回传 endpoint —— 前端 renderMcpList / renderMcpSettings 读的就是这个字段。
	items := getPlugins(t, client, ts.URL)
	var got *pluginsListItem
	for i := range items {
		if items[i].Name == "remote_test" {
			got = &items[i]
			break
		}
	}
	if got == nil {
		t.Fatal("插件列表中找不到刚添加的 remote_test")
	}
	if got.Type != "mcp-http" {
		t.Errorf("type 期望 mcp-http，实际 %q", got.Type)
	}
	if got.Endpoint != mcp.URL {
		t.Errorf("endpoint 期望 %q，实际 %q（前端靠它展示远程端点）", mcp.URL, got.Endpoint)
	}
	if !got.Configured {
		t.Error("新添加的插件应为已启用（configured）")
	}
	if !got.Enabled {
		t.Error("工具已加载，enabled 应为 true")
	}

	// 落盘：plugins.yaml 记下 type 与 endpoint，重启后不丢。
	raw, err := os.ReadFile(filepath.Join(cfgDir, "plugins.yaml"))
	if err != nil {
		t.Fatalf("插件配置未写回 plugins.yaml: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"name: remote_test", "type: mcp-http", "endpoint: " + mcp.URL} {
		if !strings.Contains(text, want) {
			t.Errorf("plugins.yaml 缺少 %q，实际内容：\n%s", want, text)
		}
	}
}

// 新增表单的按类型校验：远程要 http(s) 端点、stdio 要启动命令，错误必须可读且为 400。
func TestPluginsAddValidation(t *testing.T) {
	deps := newTestDepsAt(t, t.TempDir())
	ts := newTestPluginsServer(t, deps)
	client := withAuthClient(t, ts)

	cases := []struct {
		label string
		body  map[string]any
		hint  string
	}{
		{"远程缺端点", map[string]any{"name": "p1", "type": "mcp-http"}, "端点"},
		{"远程无协议", map[string]any{"name": "p2", "type": "mcp-http", "endpoint": "search.parallel.ai/mcp"}, "http"},
		{"远程错协议", map[string]any{"name": "p3", "type": "mcp-http", "endpoint": "ftp://example.com/mcp"}, "http"},
		{"stdio 缺命令", map[string]any{"name": "p4", "type": "mcp"}, "启动命令"},
		{"未知类型", map[string]any{"name": "p5", "type": "grpc", "command": "x"}, "不支持的类型"},
		{"缺名称", map[string]any{"type": "mcp", "command": "npx"}, "名称"},
	}
	for _, c := range cases {
		resp := postJSON(t, client, ts.URL+"/api/plugins", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s：期望 400，实际 %d", c.label, resp.StatusCode)
			continue
		}
		var b map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
			t.Errorf("%s：解析错误响应失败: %v", c.label, err)
			continue
		}
		msg, _ := b["error"].(string)
		if !strings.Contains(msg, c.hint) {
			t.Errorf("%s：错误信息应含 %q，实际 %q", c.label, c.hint, msg)
		}
	}

	// 校验失败不应留下任何条目（避免半成品插件写进 plugins.yaml）。
	if items := getPlugins(t, client, ts.URL); len(items) != 0 {
		t.Errorf("校验失败不应写入插件，实际有 %d 条: %v", len(items), items)
	}
}
