package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"codeforge/config"
)

// 读取 GET /api/subagents 的响应。
func getSubagentPrefs(t *testing.T, client *http.Client, base string) map[string]any {
	t.Helper()
	resp, err := client.Get(base + "/api/subagents")
	if err != nil {
		t.Fatalf("GET /api/subagents 失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/subagents 期望 200，实际 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return body
}

func boolField(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("字段 %q 期望布尔，实际 %T(%v)", key, m[key], m[key])
	}
	return v
}

func intField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("字段 %q 期望数值，实际 %T(%v)", key, m[key], m[key])
	}
	return int(v)
}

// 设置页「子智能体」的读写闭环：默认值 → 部分更新（指针语义）→ 越界收敛 → 落盘。
func TestSubagentPrefsRoundTrip(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)

	var appliedCount int
	var pluginID string
	var pluginOn bool
	srv := deps.newServer()
	srv.SetBuiltinPluginApply(func(id string, on bool) { pluginID, pluginOn = id, on })
	srv.SetSubagentApply(func() { appliedCount++ })

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	// 1) 默认值：开关开、并发为硬上限、能力三项全开。
	got := getSubagentPrefs(t, client, ts.URL)
	if !boolField(t, got, "enabled") {
		t.Error("子智能体总开关默认应为开启")
	}
	if n := intField(t, got, "max_concurrent"); n != config.SubagentConcurrencyCap {
		t.Errorf("默认并发期望 %d，实际 %d", config.SubagentConcurrencyCap, n)
	}
	if n := intField(t, got, "max_allowed"); n != config.SubagentConcurrencyCap {
		t.Errorf("max_allowed 期望 %d，实际 %d", config.SubagentConcurrencyCap, n)
	}
	for _, k := range []string{"allow_write", "allow_delete", "allow_memory"} {
		if !boolField(t, got, k) {
			t.Errorf("%s 默认应为开启", k)
		}
	}

	// 2) 只改并发：其余字段（未出现在请求体里）必须原样保留 —— 这是指针语义的关键。
	resp := postJSON(t, client, ts.URL+"/api/subagents", map[string]any{"max_concurrent": 2})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST 期望 200，实际 %d", resp.StatusCode)
	}
	got = getSubagentPrefs(t, client, ts.URL)
	if n := intField(t, got, "max_concurrent"); n != 2 {
		t.Errorf("并发期望 2，实际 %d", n)
	}
	for _, k := range []string{"allow_write", "allow_delete", "allow_memory"} {
		if !boolField(t, got, k) {
			t.Errorf("只改并发时 %s 不应被改掉（指针语义）", k)
		}
	}
	if !boolField(t, got, "enabled") {
		t.Error("只改并发时总开关不应被改掉")
	}
	if appliedCount != 1 {
		t.Errorf("每次 POST 都应触发运行时应用钩子一次，实际 %d", appliedCount)
	}

	// 3) 关掉能力三项：并发保持上一步的 2。
	resp = postJSON(t, client, ts.URL+"/api/subagents", map[string]any{
		"allow_write":  false,
		"allow_delete": false,
		"allow_memory": false,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST 期望 200，实际 %d", resp.StatusCode)
	}
	got = getSubagentPrefs(t, client, ts.URL)
	for _, k := range []string{"allow_write", "allow_delete", "allow_memory"} {
		if boolField(t, got, k) {
			t.Errorf("%s 应已被关闭", k)
		}
	}
	if n := intField(t, got, "max_concurrent"); n != 2 {
		t.Errorf("关能力不应影响并发，期望 2，实际 %d", n)
	}

	// 4) 越界并发：收敛到硬上限而不是报错（手改 local.yaml 写 99 时不至于卡死）。
	resp = postJSON(t, client, ts.URL+"/api/subagents", map[string]any{"max_concurrent": 99})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("越界并发期望 200（收敛而非拒绝），实际 %d", resp.StatusCode)
	}
	got = getSubagentPrefs(t, client, ts.URL)
	if n := intField(t, got, "max_concurrent"); n != config.SubagentConcurrencyCap {
		t.Errorf("越界并发期望收敛到 %d，实际 %d", config.SubagentConcurrencyCap, n)
	}

	// 5) 总开关：与内置插件同一真源，走同一套运行时应用钩子。
	resp = postJSON(t, client, ts.URL+"/api/subagents", map[string]any{"enabled": false})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST enabled 期望 200，实际 %d", resp.StatusCode)
	}
	got = getSubagentPrefs(t, client, ts.URL)
	if boolField(t, got, "enabled") {
		t.Error("总开关应已被关闭")
	}
	if pluginID != "multi_agent" {
		t.Errorf("总开关应委托内置插件钩子（id=multi_agent），实际 id=%q", pluginID)
	}
	if pluginOn {
		t.Error("关闭总开关时插件钩子应收到 on=false")
	}
	if deps.cfg.BuiltinPlugins.MultiAgentEnabled() {
		t.Error("cfg.BuiltinPlugins.MultiAgent 应与总开关同源变为 false")
	}

	// 6) 落盘：state.yaml 记下 enabled / subagents 两节，重启后不丢。
	//    本轮共 4 次 POST，每次都应触发一次运行时应用钩子。
	if appliedCount != 4 {
		t.Errorf("4 次 POST 应触发 4 次应用钩子，实际 %d", appliedCount)
	}
	path := config.StatePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("设置未写回 state.yaml: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"multi_agent:", "subagents:", "max_concurrent:", "allow_write:", "allow_delete:", "allow_memory:"} {
		if !strings.Contains(text, want) {
			t.Errorf("state.yaml 缺少 %q，实际内容：\n%s", want, text)
		}
	}
	// 运行状态不得再把用户/代码维护的条目抄进覆盖文件。
	for _, banned := range []string{"plugins:", "rules:", "deny_patterns:"} {
		for _, ln := range strings.Split(text, "\n") {
			if strings.HasPrefix(ln, banned) {
				t.Errorf("state.yaml 不该带用户侧字段 %q:\n%s", banned, text)
			}
		}
	}

	// 7) 重新加载配置文件：设置项确实落盘而非只活在内存。
	reloaded, err := config.Load(cfgDir)
	if err != nil {
		t.Fatalf("重新加载配置失败: %v", err)
	}
	if reloaded.BuiltinPlugins.MultiAgentEnabled() {
		t.Error("重载后总开关应为关闭")
	}
	if got := reloaded.SubagentMaxConcurrent(); got != config.SubagentConcurrencyCap {
		t.Errorf("重载后并发期望 %d（越界值已按硬上限落盘），实际 %d", config.SubagentConcurrencyCap, got)
	}
	if reloaded.SubagentAllowWrite() || reloaded.SubagentAllowDelete() || reloaded.SubagentAllowMemory() {
		t.Error("重载后能力三项应保持关闭")
	}
}

// 方法限制与坏请求体：只接受 GET/POST，坏 JSON 给 400 而不是 500。
func TestSubagentPrefsMethodAndBadBody(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/api/subagents", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE 期望 405，实际 %d", resp.StatusCode)
	}

	badResp, err := client.Post(ts.URL+"/api/subagents", "application/json", strings.NewReader("{不是 JSON"))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Errorf("坏请求体期望 400，实际 %d", badResp.StatusCode)
	}
}
