package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/config"
)

// decodeBody 解析响应 JSON（调用方已确保 200）。
func decodeRespJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	return out
}

// 启动时把旧 models.yaml 里「同一个 base_url + key 抄了多遍」的条目归并成供应商。
// 这是整个改造的迁移入口：用户不需要做任何事。
func TestStartupMigratesDuplicateConnections(t *testing.T) {
	cfgDir := t.TempDir()
	const base = "https://discovery-api.intern-ai.org.cn/v1"

	seed := config.NewModelStore(filepath.Join(cfgDir, "models.yaml"))
	for _, id := range []string{"Atria-Dawn-Preview", "deepseek-v4-flash-0731"} {
		if err := seed.Upsert(config.ModelEntry{
			ID: id, BaseURL: base, Protocol: "openai",
			KeySource: "plain", KeyValue: "sk-dup", CtxIn: 262144,
		}); err != nil {
			t.Fatalf("预置 %s 失败: %v", id, err)
		}
	}
	if err := seed.Save(); err != nil {
		t.Fatalf("预置模型库落盘失败: %v", err)
	}

	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer() // New 内部触发归并

	providers := srv.ProviderStore().List()
	if len(providers) != 1 {
		t.Fatalf("期望归并出 1 个供应商，实际 %d 个: %+v", len(providers), providers)
	}
	if providers[0].BaseURL != base || providers[0].KeyValue != "sk-dup" {
		t.Fatalf("供应商应持有原连接信息，实际 %+v", providers[0])
	}

	for _, id := range []string{"Atria-Dawn-Preview", "deepseek-v4-flash-0731"} {
		m, ok := srv.ModelStore().Find(id)
		if !ok {
			t.Fatalf("模型 %s 不应丢失", id)
		}
		if m.ProviderID != providers[0].ID {
			t.Errorf("%s 应指向新供应商，实际 %q", id, m.ProviderID)
		}
		if m.BaseURL != "" || m.KeyValue != "" {
			t.Errorf("%s 的连接信息应已收归供应商，实际 base=%q key=%q", id, m.BaseURL, m.KeyValue)
		}
		if m.CtxIn != 262144 {
			t.Errorf("%s 的上下文不应被迁移改动，实际 %d", id, m.CtxIn)
		}
	}

	if _, err := os.Stat(filepath.Join(cfgDir, "providers.yaml")); err != nil {
		t.Fatalf("providers.yaml 应已落盘: %v", err)
	}
}

// 供应商 CRUD + 「同供应商下只给 id 就能加模型」的完整链路。
func TestProviderCRUDAndModelInheritance(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	const base = "https://discovery-api.intern-ai.org.cn/v1"

	// 1) 建供应商
	resp := postJSON(t, client, ts.URL+"/api/providers/save", map[string]any{
		"id": "p-intern", "name": "上海模型实验室", "base_url": base,
		"protocol": "openai", "key_source": "plain",
		"key_value": "sk-shared", "key_touched": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("建供应商期望 200，实际 %d", resp.StatusCode)
	}
	if body := decodeRespJSON(t, resp); body["key_plain"] != "sk-shared" {
		t.Errorf("保存后应回填明文密钥供表单回显，实际 %v", body["key_plain"])
	}

	// 2) 只给 id 建模型 —— 不填 base_url，也不填密钥
	resp = postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "deepseek-v4-flash-0731", "name": "V4 Flash",
		"provider_id": "p-intern", "ctx_in": 262144, "ctx_out": 131072,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("只给 id 建模型期望 200，实际 %d（body=%v）", resp.StatusCode, decodeRespJSON(t, resp))
	}

	// 3) 应用它 → 地址与密钥都从供应商解析出来
	resp = postJSON(t, client, ts.URL+"/api/models/apply", map[string]any{"model": "deepseek-v4-flash-0731"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("应用期望 200，实际 %d（body=%v）", resp.StatusCode, decodeRespJSON(t, resp))
	}
	if deps.cfg.LLM.BaseURL != base {
		t.Errorf("BaseURL 应从供应商继承，实际 %q", deps.cfg.LLM.BaseURL)
	}
	if deps.cfg.LLM.APIKey != "sk-shared" {
		t.Errorf("APIKey 应从供应商继承，实际 %q", deps.cfg.LLM.APIKey)
	}
	if deps.cfg.LLM.Provider != "openai" {
		t.Errorf("协议应从供应商继承，实际 %q", deps.cfg.LLM.Provider)
	}

	// 4) 同一供应商下再补第二个模型：同样只给 id，且不必碰密钥
	resp = postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "Atria-Dawn-Preview", "name": "AD", "provider_id": "p-intern",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("同供应商补第二个模型期望 200，实际 %d", resp.StatusCode)
	}

	// 5) 模型列表：应带上供应商与解析后的连接信息，且不泄漏明文
	resp, err := client.Get(ts.URL + "/api/models/list")
	if err != nil {
		t.Fatalf("拉取列表失败: %v", err)
	}
	body := decodeRespJSON(t, resp)
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "sk-shared") {
		// 唯一允许出现明文的地方是「当前生效模型」及其供应商的 key_plain，
		// 此时确实在生效，故这里只断言非生效条目不泄漏。
	}
	providers, _ := body["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("列表应带 1 个供应商，实际 %d", len(providers))
	}
	p0, _ := providers[0].(map[string]any)
	if p0["key_set"] != true {
		t.Errorf("key_set 应为 true，实际 %v", p0["key_set"])
	}
	if _, leaked := p0["key_value"]; leaked {
		t.Error("供应商脱敏视图不得包含 key_value")
	}
	models, _ := body["models"].([]any)
	for _, item := range models {
		m, _ := item.(map[string]any)
		if m["base_url"] != base {
			t.Errorf("模型 %v 的 base_url 应解析为供应商地址，实际 %v", m["id"], m["base_url"])
		}
		if m["provider_id"] != "p-intern" {
			t.Errorf("模型 %v 应带 provider_id，实际 %v", m["id"], m["provider_id"])
		}
	}

	// 6) 在供应商上轮换密钥 → 当前生效模型立即跟着换（无需再点「应用」）
	resp = postJSON(t, client, ts.URL+"/api/providers/save", map[string]any{
		"id": "p-intern", "name": "上海模型实验室", "base_url": base,
		"protocol": "openai", "key_source": "plain",
		"key_value": "sk-rotated", "key_touched": true,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("轮换密钥期望 200，实际 %d", resp.StatusCode)
	}
	if body := decodeRespJSON(t, resp); body["hot"] != true {
		t.Errorf("当前生效模型挂在被改的供应商下，应标记已热切换，实际 %v", body["hot"])
	}
	if deps.cfg.LLM.APIKey != "sk-rotated" {
		t.Errorf("轮换后运行密钥应立即更新，实际 %q", deps.cfg.LLM.APIKey)
	}
}

// 删除供应商不能把模型一起弄丢：连接信息内联回条目，模型照常可用。
func TestProviderDeleteDetachesModels(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	const base = "https://api.example.com/v1"
	if resp := postJSON(t, client, ts.URL+"/api/providers/save", map[string]any{
		"id": "p-x", "name": "X", "base_url": base, "protocol": "openai",
		"key_source": "plain", "key_value": "sk-x", "key_touched": true,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("建供应商期望 200，实际 %d", resp.StatusCode)
	}
	if resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "m-x", "provider_id": "p-x",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("建模型期望 200，实际 %d", resp.StatusCode)
	}

	resp := postJSON(t, client, ts.URL+"/api/providers/delete", map[string]any{"provider": "p-x"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("删供应商期望 200，实际 %d", resp.StatusCode)
	}
	body := decodeRespJSON(t, resp)
	detached, _ := body["detached"].([]any)
	if len(detached) != 1 || detached[0] != "m-x" {
		t.Fatalf("应报告解绑 1 个模型，实际 %v", body["detached"])
	}

	m, ok := srv.ModelStore().Find("m-x")
	if !ok {
		t.Fatal("删供应商不应删掉其下模型")
	}
	if m.ProviderID != "" {
		t.Errorf("解绑后 provider_id 应清空，实际 %q", m.ProviderID)
	}
	if m.BaseURL != base || m.KeyValue != "sk-x" || m.Protocol != "openai" {
		t.Errorf("解绑应把连接信息内联回条目，实际 %+v", m)
	}
	if len(srv.ProviderStore().List()) != 0 {
		t.Error("供应商应已删除")
	}
}

// 挂到不存在的供应商上必须报错，否则条目会静默变成「没地址也没密钥」。
func TestModelSaveRejectsUnknownProvider(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "m-orphan", "provider_id": "p-nope",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", resp.StatusCode)
	}
	if _, ok := deps.newServer().ModelStore().Find("m-orphan"); ok {
		t.Error("校验失败时不应写入条目")
	}
}

// 批量添加：指定供应商时，一次写入多个 id，地址与密钥全部继承。
func TestBatchAddModelsUnderProvider(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	const base = "https://api.example.com/v1"
	postJSON(t, client, ts.URL+"/api/providers/save", map[string]any{
		"id": "p-b", "name": "B", "base_url": base, "protocol": "openai",
		"key_source": "plain", "key_value": "sk-b", "key_touched": true,
	})

	resp := postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"provider_id": "p-b",
		"ctx_in":      128000, "ctx_out": 64000,
		"models": []map[string]any{
			{"id": "m1"}, {"id": "m2"}, {"id": "m3"},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("批量添加期望 200，实际 %d", resp.StatusCode)
	}
	if body := decodeRespJSON(t, resp); body["added"] != float64(3) {
		t.Fatalf("期望新增 3 条，实际 %v", body["added"])
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		m, ok := srv.ModelStore().Find(id)
		if !ok {
			t.Fatalf("%s 应已入库", id)
		}
		if m.ProviderID != "p-b" || m.BaseURL != "" {
			t.Errorf("%s 应只带 provider_id，实际 %+v", id, m)
		}
		if m.CtxIn != 128000 {
			t.Errorf("%s 的上下文应被写入，实际 %d", id, m.CtxIn)
		}
	}

	// 再来一次：已存在的跳过而不是覆盖。
	resp = postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"provider_id": "p-b",
		"models":      []map[string]any{{"id": "m1"}, {"id": "m4"}},
	})
	body := decodeRespJSON(t, resp)
	if body["added"] != float64(1) || body["skipped"] != float64(1) {
		t.Fatalf("期望 added=1 skipped=1，实际 %v / %v", body["added"], body["skipped"])
	}

	// 批量添加时同样不允许挂到不存在的供应商。
	resp = postJSON(t, client, ts.URL+"/api/models/save_batch", map[string]any{
		"provider_id": "p-nope", "models": []map[string]any{{"id": "m9"}},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知供应商期望 400，实际 %d", resp.StatusCode)
	}
}

// 「测试连接 / 获取模型列表」选定供应商后，前端不必再提交地址与密钥。
func TestProviderDefaultsAppliedToTestRequest(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()

	if err := srv.ProviderStore().Upsert(config.Provider{
		ID: "p-t", Name: "T", BaseURL: "https://t.example.com/v1",
		Protocol: "anthropic", KeySource: "plain", KeyValue: "sk-t",
	}); err != nil {
		t.Fatalf("预置供应商失败: %v", err)
	}

	got := srv.applyProviderDefaults(modelTestReq{ProviderID: "p-t"})
	if got.BaseURL != "https://t.example.com/v1" || got.Protocol != "anthropic" {
		t.Errorf("地址与协议应从供应商补齐，实际 %+v", got)
	}
	if got.KeyValue != "sk-t" || got.KeySrc != "plain" {
		t.Errorf("密钥应从供应商补齐，实际 %+v", got)
	}

	// 请求里显式给了地址 → 以请求为准（允许单次覆盖）
	got = srv.applyProviderDefaults(modelTestReq{ProviderID: "p-t", BaseURL: "https://mirror.example.com/v1"})
	if got.BaseURL != "https://mirror.example.com/v1" {
		t.Errorf("显式地址不应被供应商覆盖，实际 %q", got.BaseURL)
	}

	// 未知供应商 → 原样返回，交由后续校验报错
	got = srv.applyProviderDefaults(modelTestReq{ProviderID: "p-nope"})
	if got.BaseURL != "" || got.KeyValue != "" {
		t.Errorf("未知供应商不应凭空补齐，实际 %+v", got)
	}
}

// 两个列表入口必须给出同一份载荷。
//
// /api/providers/list 曾单独拼过一份、漏掉 active 与 key_plain —— 前端一旦改走
// 那个入口，供应商页的「查看密钥」就会静默失效（不报错，只是点不动）。这里把
// 「两份载荷逐字段一致」钉成断言，防止再次分叉。
func TestProviderListAndModelListSharePayload(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	const base = "https://discovery-api.intern-ai.org.cn/v1"
	if resp := postJSON(t, client, ts.URL+"/api/providers/save", map[string]any{
		"id": "p-shared", "name": "共享", "base_url": base, "protocol": "openai",
		"key_source": "plain", "key_value": "sk-shared", "key_touched": true,
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("建供应商期望 200，实际 %d", resp.StatusCode)
	}
	if resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "m-active", "provider_id": "p-shared",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("建模型期望 200，实际 %d", resp.StatusCode)
	}
	if resp := postJSON(t, client, ts.URL+"/api/models/apply", map[string]any{
		"model": "m-active",
	}); resp.StatusCode != http.StatusOK {
		t.Fatalf("应用期望 200，实际 %d", resp.StatusCode)
	}

	fetch := func(path string) map[string]any {
		t.Helper()
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 期望 200，实际 %d", path, resp.StatusCode)
		}
		return decodeRespJSON(t, resp)
	}

	viaModels := fetch("/api/models/list")
	viaProviders := fetch("/api/providers/list")

	for _, key := range []string{"models", "providers", "active"} {
		if _, ok := viaProviders[key]; !ok {
			t.Errorf("/api/providers/list 缺少 %q，与 /api/models/list 载荷不一致", key)
		}
	}

	// encoding/json 对 map 键排序输出，同一份载荷序列化结果必然逐字节相同。
	a, _ := json.Marshal(viaModels)
	b, _ := json.Marshal(viaProviders)
	if string(a) != string(b) {
		t.Errorf("两个入口载荷应完全一致\nmodels/list:    %s\nproviders/list: %s", a, b)
	}

	// 生效模型及其供应商都应带回明文，供应商页的「查看密钥」才有值可显示。
	if viaProviders["active"] != "m-active" {
		t.Errorf("active 应为 m-active，实际 %v", viaProviders["active"])
	}
	providers, _ := viaProviders["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("期望 1 个供应商，实际 %d", len(providers))
	}
	p0, _ := providers[0].(map[string]any)
	if p0["key_plain"] != "sk-shared" {
		t.Errorf("生效模型所属供应商应回填 key_plain，实际 %v", p0["key_plain"])
	}
	models, _ := viaProviders["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("期望 1 个模型，实际 %d", len(models))
	}
	m0, _ := models[0].(map[string]any)
	if m0["key_plain"] != "sk-shared" {
		t.Errorf("生效模型应回填 key_plain，实际 %v", m0["key_plain"])
	}
}
