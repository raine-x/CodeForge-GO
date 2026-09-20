package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeforge/config"
)

// ---------- 纯解析：各家 /models 响应形态 ----------

func idsOf(list []discoveredModel) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func assertIDs(t *testing.T, got []discoveredModel, want []string) {
	t.Helper()
	ids := idsOf(got)
	if len(ids) != len(want) {
		t.Fatalf("解析结果期望 %v，实际 %v", want, ids)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Fatalf("解析结果期望 %v，实际 %v", want, ids)
		}
	}
}

func TestParseModelListForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "OpenAI data + id",
			raw:  `{"object":"list","data":[{"id":"gpt-4o","object":"model"},{"id":"gpt-4o-mini"}]}`,
			want: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			name: "Anthropic data + display_name",
			raw:  `{"data":[{"id":"claude-sonnet-4","display_name":"Claude Sonnet 4"},{"id":"claude-haiku"}]}`,
			want: []string{"claude-sonnet-4", "claude-haiku"},
		},
		{
			name: "Ollama models + name",
			raw:  `{"models":[{"name":"llama3:8b"},{"name":"qwen2.5:7b"}]}`,
			want: []string{"llama3:8b", "qwen2.5:7b"},
		},
		{
			name: "裸数组字符串",
			raw:  `["gpt-4o","glm-4.6"]`,
			want: []string{"gpt-4o", "glm-4.6"},
		},
		{
			name: "裸数组对象（走 model 字段）",
			raw:  `[{"model":"glm-4.6","title":"GLM 4.6"}]`,
			want: []string{"glm-4.6"},
		},
		{
			name: "多层信封 items",
			raw:  `{"result":{"items":[{"id":"a"},{"id":"b"}]}}`,
			want: []string{"a", "b"},
		},
		{
			name: "无命名数组兜底",
			raw:  `{"whatever":[{"id":"x1"}]}`,
			want: []string{"x1"},
		},
		{
			name: "slug 兜底",
			raw:  `{"data":[{"slug":"z-ai/glm-5.3-free"}]}`,
			want: []string{"z-ai/glm-5.3-free"},
		},
		{
			name: "大小写去重（保留首次出现）",
			raw:  `{"data":[{"id":"GPT-4o"},{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`,
			want: []string{"GPT-4o", "gpt-4o-mini"},
		},
		{
			name: "混入无 id 的脏元素被跳过",
			raw:  `{"data":[{"object":"model"},{"id":"  "},{"id":"ok"},123,"s2"]}`,
			want: []string{"ok", "s2"},
		},
		{
			name: "id 带空白会被去除",
			raw:  `{"data":[{"id":"  glm-4.6  ","name":"GLM"}]}`,
			want: []string{"glm-4.6"},
		},
		{name: "空数组", raw: `{"data":[]}`, want: nil},
		{name: "空对象", raw: `{}`, want: nil},
		{name: "坏 JSON", raw: `{"data":[`, want: nil},
		{name: "HTML 错误页（网关 200 但返回网页）", raw: `<html><body>Not Found</body></html>`, want: nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertIDs(t, parseModelList([]byte(c.raw)), c.want)
		})
	}
}

// 显示名抽取规则：id 取自 name 时不再重复展示为显示名（前端避免「id 和名字一样」）。
func TestParseModelListNames(t *testing.T) {
	list := parseModelList([]byte(`{"data":[
		{"id":"claude-sonnet-4","display_name":"Claude Sonnet 4"},
		{"id":"glm-4.6","name":"GLM 4.6"},
		{"name":"llama3:8b"}
	]}`))
	if len(list) != 3 {
		t.Fatalf("期望 3 条，实际 %d", len(list))
	}
	if list[0].ID != "claude-sonnet-4" || list[0].Name != "Claude Sonnet 4" {
		t.Errorf("display_name 未取到：%+v", list[0])
	}
	if list[1].ID != "glm-4.6" || list[1].Name != "GLM 4.6" {
		t.Errorf("name 未作为显示名带上：%+v", list[1])
	}
	if list[2].ID != "llama3:8b" || list[2].Name != "" {
		t.Errorf("id 取自 name 时显示名应留空：%+v", list[2])
	}
}

// 畸形嵌套不得无限递归（最多向下探 4 层）。
func TestFindModelArrayDepthLimit(t *testing.T) {
	var v any
	deep := `{"a":{"b":{"c":{"d":{"e":[{"id":"x"}]}}}}}`
	if err := json.Unmarshal([]byte(deep), &v); err != nil {
		t.Fatalf("测试数据不是合法 JSON: %v", err)
	}
	if got := findModelArray(v, 0); got != nil {
		t.Errorf("超过深度上限应返回 nil，实际 %v", got)
	}
}

// ---------- 模型库路径守卫（落盘污染事故的回归测试） ----------

// ConfigDir 为空时模型库必须退化为纯内存库，绝不拼出相对路径写进工作目录。
func TestModelStorePathGuardsEmptyConfigDir(t *testing.T) {
	if got := modelStorePath(config.Default()); got != "" {
		t.Fatalf("无配置目录时期望空路径（纯内存库），实际 %q", got)
	}
	dir := t.TempDir()
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("加载测试配置失败: %v", err)
	}
	want := filepath.Join(dir, "models.yaml")
	if got := modelStorePath(cfg); got != want {
		t.Fatalf("有配置目录时期望 %q，实际 %q", want, got)
	}
}

// ---------- 上游 /models 拉取 ----------

// fakeModelsServer 模拟上游网关：固定状态码 + 固定响应体。
func fakeModelsServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// newDiscoverServer 起一个带 Cookie 鉴权的测试服务，返回 (客户端, base URL)。
func newDiscoverServer(t *testing.T, deps *testDeps) (*http.Client, string) {
	t.Helper()
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	return withAuthClient(t, ts), ts.URL
}

func TestModelDiscoverHappyPath(t *testing.T) {
	upstream := fakeModelsServer(t, `{"object":"list","data":[{"id":"glm-4.6"},{"id":"glm-4.5"},{"id":"gpt-4o"}]}`, http.StatusOK)

	deps := newTestDeps(t)
	srv := deps.newServer()
	// 库中已有 glm-4.6：应被标记 in_library，前端据此禁止重复勾选。
	if err := srv.ModelStore().Upsert(config.ModelEntry{ID: "glm-4.6", Protocol: "openai"}); err != nil {
		t.Fatalf("预置模型库条目失败: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	resp := postJSON(t, client, ts.URL+"/api/models/discover", map[string]any{
		"base_url":  upstream.URL,
		"protocol":  "openai",
		"key_value": "sk-test",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	var body struct {
		OK            bool              `json:"ok"`
		Count         int               `json:"count"`
		Endpoint      string            `json:"endpoint"`
		KeyFromActive bool              `json:"key_from_active"`
		Models        []discoveredModel `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !body.OK {
		t.Fatalf("期望 ok=true，实际 %+v", body)
	}
	if body.Count != 3 {
		t.Errorf("count 期望 3，实际 %d", body.Count)
	}
	if body.Endpoint != upstream.URL+"/models" {
		t.Errorf("endpoint 期望 %s/models，实际 %s", upstream.URL, body.Endpoint)
	}
	if body.KeyFromActive {
		t.Error("请求体已带 key_value，不应标记为「用了当前生效配置的密钥」")
	}
	// 排序：不分大小写升序 → glm-4.5, glm-4.6, gpt-4o
	assertIDs(t, body.Models, []string{"glm-4.5", "glm-4.6", "gpt-4o"})
	for _, m := range body.Models {
		if m.ID == "glm-4.6" && !m.InLibrary {
			t.Error("库中已有的 glm-4.6 应标记 in_library=true")
		}
		if m.ID != "glm-4.6" && m.InLibrary {
			t.Errorf("库中没有的 %s 不应标记 in_library", m.ID)
		}
	}
}

// base_url 未带版本段且 /models 404 时，应自动回退到 /v1/models。
func TestModelDiscoverFallsBackToV1Path(t *testing.T) {
	var sawPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPaths = append(sawPaths, r.URL.Path)
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m-v1"}]}`))
	}))
	t.Cleanup(upstream.Close)

	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	resp := postJSON(t, client, base+"/api/models/discover", map[string]any{
		"base_url":  upstream.URL,
		"protocol":  "openai",
		"key_value": "sk-test",
	})
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != true {
		t.Fatalf("回退后应成功，实际 %+v", body)
	}
	if body["endpoint"] != upstream.URL+"/v1/models" {
		t.Errorf("endpoint 期望 %s/v1/models，实际 %v", upstream.URL, body["endpoint"])
	}
	if len(sawPaths) != 2 || sawPaths[0] != "/models" || sawPaths[1] != "/v1/models" {
		t.Errorf("应先试 /models 再回退 /v1/models，实际 %v", sawPaths)
	}
}

// 鉴权失败不换路径重试（换路径没有意义），并把状态码翻译成人话。
func TestModelDiscoverDoesNotRetryOnAuthError(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	t.Cleanup(upstream.Close)

	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	resp := postJSON(t, client, base+"/api/models/discover", map[string]any{
		"base_url":  upstream.URL,
		"protocol":  "openai",
		"key_value": "sk-bad",
	})
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != false {
		t.Fatalf("401 时应返回 ok=false，实际 %+v", body)
	}
	if msg := asString(body["error"]); !strings.Contains(msg, "401") || !strings.Contains(msg, "密钥") {
		t.Errorf("错误文案应说明密钥问题并带状态码，实际 %q", msg)
	}
	if hits != 1 {
		t.Errorf("鉴权失败不应换路径重试，实际请求了 %d 次", hits)
	}
}

// 上游返回 200 但内容是网页 / 空列表：给出可读错误而不是空列表。
func TestModelDiscoverUnparsableBody(t *testing.T) {
	upstream := fakeModelsServer(t, `<html>oops</html>`, http.StatusOK)

	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	resp := postJSON(t, client, base+"/api/models/discover", map[string]any{
		"base_url":  upstream.URL,
		"protocol":  "openai",
		"key_value": "sk-test",
	})
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != false {
		t.Fatalf("无法解析时期望 ok=false，实际 %+v", body)
	}
	if msg := asString(body["error"]); !strings.Contains(msg, "无法识别") {
		t.Errorf("错误文案应说明格式无法识别，实际 %q", msg)
	}
}

// 表单没填地址/密钥：给出明确提示，而不是发一个必然失败的请求出去。
func TestModelDiscoverMissingInput(t *testing.T) {
	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	var body map[string]any

	// 1) 地址与生效配置都为空 → 提示补地址。
	resp := postJSON(t, client, base+"/api/models/discover", map[string]any{})
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != false || !strings.Contains(asString(body["error"]), "请求地址") {
		t.Errorf("空地址应提示补请求地址，实际 %+v", body)
	}

	// 2) 有地址但没密钥 → 提示补密钥。
	resp = postJSON(t, client, base+"/api/models/discover", map[string]any{
		"base_url": "https://api.example.com/v1",
	})
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != false || !strings.Contains(asString(body["error"]), "Key") {
		t.Errorf("空密钥应提示补密钥，实际 %+v", body)
	}
}

// 表单留空时回退到「当前生效」的配置，并如实标记 key_from_active（避免「用了哪个密钥」不可见）。
func TestModelDiscoverFallsBackToActiveConfig(t *testing.T) {
	upstream := fakeModelsServer(t, `{"data":[{"id":"m1"}]}`, http.StatusOK)

	deps := newTestDeps(t)
	deps.cfg.LLM.BaseURL = upstream.URL
	deps.cfg.LLM.APIKey = "sk-active"
	client, base := newDiscoverServer(t, deps)

	resp := postJSON(t, client, base+"/api/models/discover", map[string]any{"protocol": "openai"})
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["ok"] != true {
		t.Fatalf("应回退到当前生效配置，实际 %+v", body)
	}
	if body["key_from_active"] != true {
		t.Errorf("key_from_active 应为 true，实际 %+v", body["key_from_active"])
	}
}

func TestModelDiscoverMethodNotAllowed(t *testing.T) {
	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	resp, err := client.Get(base + "/api/models/discover")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 期望 405，实际 %d", resp.StatusCode)
	}
}

func TestModelDiscoverReason(t *testing.T) {
	cases := map[int]string{
		401: "密钥", 403: "权限", 404: "404", 405: "405", 429: "限流",
		500: "500", 502: "502", 503: "503", 504: "504", 418: "418",
	}
	for status, want := range cases {
		if got := modelDiscoverReason(status); !strings.Contains(got, want) {
			t.Errorf("modelDiscoverReason(%d) = %q，应包含 %q", status, got, want)
		}
	}
}

// ---------- 批量添加 ----------

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	return string(raw)
}

func TestModelSaveBatchAddsAndSkipsExisting(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()

	// 预先存在的条目：用户手调过名字/地址/密钥，批量添加绝不能覆盖。
	if err := srv.ModelStore().Upsert(config.ModelEntry{
		ID: "glm-4.6", Name: "手调过的名字", BaseURL: "https://mine.example.com/v1",
		Protocol: "openai", KeySource: "plain", KeyValue: "sk-mine",
	}); err != nil {
		t.Fatalf("预置条目失败: %v", err)
	}

	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	resp := postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"models": []map[string]any{
			{"id": "glm-4.6", "name": "上游给的名字"},
			{"id": "glm-4.5"},
			{"id": "  "}, // 空 id 直接忽略（不计入 added/skipped/failed）
			{"id": "gpt-4o", "name": "GPT-4o"},
		},
		"base_url":   "https://gateway.example.com/v1",
		"protocol":   "custom", // 历史值，应归一化为 openai
		"key_source": "plain",
		"key_value":  "sk-batch",
		"ctx_out":    131072,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批量添加期望 200，实际 %d", resp.StatusCode)
	}
	raw := readBody(t, resp)

	// 响应必须是脱敏视图：明文 key 绝不能出现在返回体里。
	if strings.Contains(raw, "sk-batch") {
		t.Error("批量添加的响应泄露了明文 API Key")
	}

	var body struct {
		OK         bool     `json:"ok"`
		Added      int      `json:"added"`
		Skipped    int      `json:"skipped"`
		Failed     int      `json:"failed"`
		AddedIDs   []string `json:"added_ids"`
		SkippedIDs []string `json:"skipped_ids"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if !body.OK || body.Added != 2 || body.Skipped != 1 || body.Failed != 0 {
		t.Fatalf("期望 added=2 skipped=1 failed=0，实际 %+v", body)
	}
	if len(body.SkippedIDs) != 1 || body.SkippedIDs[0] != "glm-4.6" {
		t.Errorf("skipped_ids 期望 [glm-4.6]，实际 %v", body.SkippedIDs)
	}
	if len(body.AddedIDs) != 2 {
		t.Errorf("added_ids 期望 2 条，实际 %v", body.AddedIDs)
	}

	// 已有条目必须原样保留（名字/地址/密钥都不被覆盖）。
	kept, ok := srv.ModelStore().Find("glm-4.6")
	if !ok {
		t.Fatal("已有条目不应消失")
	}
	if kept.Name != "手调过的名字" || kept.BaseURL != "https://mine.example.com/v1" || kept.KeyValue != "sk-mine" {
		t.Errorf("批量添加覆盖了已有条目：%+v", kept)
	}

	// 新增条目继承本次的连接信息，协议归一化，上下文上限落到 CtxOut。
	m, ok := srv.ModelStore().Find("glm-4.5")
	if !ok {
		t.Fatal("glm-4.5 应已被添加")
	}
	if m.Protocol != "openai" {
		t.Errorf("历史 custom 应归一化为 openai，实际 %q", m.Protocol)
	}
	if m.BaseURL != "https://gateway.example.com/v1" || m.KeyValue != "sk-batch" || m.CtxOut != 131072 {
		t.Errorf("新增条目未继承连接信息：%+v", m)
	}
	if m.KeySource != "plain" {
		t.Errorf("key_source 期望 plain，实际 %q", m.KeySource)
	}

	// 落盘：一次批量添加只写一次，文件里能看到全部三条。
	disk, err := os.ReadFile(filepath.Join(cfgDir, "models.yaml"))
	if err != nil {
		t.Fatalf("模型库未落盘: %v", err)
	}
	for _, want := range []string{"glm-4.5", "gpt-4o", "glm-4.6"} {
		if !strings.Contains(string(disk), want) {
			t.Errorf("models.yaml 缺少 %q", want)
		}
	}

	// 再次批量添加同一批：全部跳过，不重复入库。
	resp2 := postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"models":   []map[string]any{{"id": "glm-4.5"}, {"id": "gpt-4o"}},
		"protocol": "openai",
		"base_url": "https://another.example.com/v1",
	})
	var body2 struct {
		Added   int `json:"added"`
		Skipped int `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp2)), &body2); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body2.Added != 0 || body2.Skipped != 2 {
		t.Errorf("重复添加应全部跳过，实际 %+v", body2)
	}
	if again, _ := srv.ModelStore().Find("glm-4.5"); again.BaseURL != "https://gateway.example.com/v1" {
		t.Errorf("重复添加不得改写已有条目地址，实际 %q", again.BaseURL)
	}
}

// 无配置目录时模型库只在内存工作：批量添加仍成功，但不落盘、不污染工作目录。
func TestModelSaveBatchWithoutConfigDirDoesNotWrite(t *testing.T) {
	deps := newTestDeps(t) // ConfigDir()=="" → 纯内存库
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	resp := postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"models":   []map[string]any{{"id": "mem-only"}},
		"protocol": "openai",
		"base_url": "https://gateway.example.com/v1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}
	if _, ok := srv.ModelStore().Find("mem-only"); !ok {
		t.Error("内存库应能读到刚添加的条目")
	}
	if srv.ModelStore().Path() != "" {
		t.Errorf("无配置目录时模型库路径应为空，实际 %q", srv.ModelStore().Path())
	}
	if _, err := os.Stat("models.yaml"); err == nil {
		t.Fatal("不得在当前工作目录写出 models.yaml（历史污染点）")
	}
}

func TestModelSaveBatchRejectsEmpty(t *testing.T) {
	deps := newTestDeps(t)
	client, base := newDiscoverServer(t, deps)

	resp := postJSON(t, client, base+"/api/models/save_batch", map[string]any{"models": []map[string]any{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("空列表期望 400，实际 %d", resp.StatusCode)
	}

	// 全部为空 id：不报错，added=0（前端误传空行时不该红脸）。
	resp = postJSON(t, client, base+"/api/models/save_batch", map[string]any{"models": []map[string]any{{"id": "  "}}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("全部空 id 时期望 200，实际 %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(readBody(t, resp)), &body); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if body["added"] != float64(0) {
		t.Errorf("空 id 不应被添加，实际 %+v", body)
	}

	getResp, err := client.Get(base + "/api/models/save_batch")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET 期望 405，实际 %d", getResp.StatusCode)
	}
}

// 超时报错必须能区分「连不通」与「上游慢」。
//
// 背景：早先无论什么原因超时都报「上游响应过慢，请稍后重试或检查网络」，
// 而用户实际遇到的往往是**地址根本连不上**（境外服务在部分网络下就是连不通）。
// 照着这句话排查会一直往「上游慢」的方向找，方向就是错的。
//
// 用 httptest 挂住不响应来**稳定**命中超时分支 —— 靠真实网络复现不了：
// 连不通时大多会立刻被拒，或被中间代理回 502，根本走不到超时。
func TestFetchModelListTimeoutMessageIsDiagnostic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // 一直挂到客户端放弃
	}))
	defer srv.Close()

	old := upstreamTimeout
	upstreamTimeout = 200 * time.Millisecond // 别真等 30 秒
	defer func() { upstreamTimeout = old }()

	ctx, cancel := context.WithTimeout(context.Background(), upstreamTimeout)
	defer cancel()

	s := &Server{}
	_, _, err := s.fetchModelList(ctx, srv.URL, "openai", "dummy")
	if err == nil {
		t.Fatal("上游挂住不响应，应当报超时")
	}
	msg := err.Error()

	if !strings.Contains(msg, "未能连上") {
		t.Fatalf("超时文案应说明「连不上」，而不只是「上游慢」，实际: %s", msg)
	}
	if strings.Contains(msg, "上游响应过慢") {
		t.Fatalf("不应再出现「上游响应过慢」这种单一归因: %s", msg)
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	if !strings.Contains(msg, host) {
		t.Fatalf("文案应带上出问题的主机名（%s），实际: %s", host, msg)
	}
	if !strings.Contains(msg, "200ms") {
		t.Fatalf("文案里的时长应取自 upstreamTimeout（200ms），不该写死 30s，实际: %s", msg)
	}
}
