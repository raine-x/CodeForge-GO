package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"codeforge/config"
)

// withAuthClient 返回一个持有有效 Cookie 的 HTTP 客户端（先请求首页换取 HttpOnly Cookie）。
func withAuthClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("创建 cookie jar 失败: %v", err)
	}
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取会话 Cookie 失败: %v", err)
	}
	resp.Body.Close()
	return client
}

func postJSON(t *testing.T, client *http.Client, url string, body map[string]any) *http.Response {
	t.Helper()
	data, _ := json.Marshal(body)
	resp, err := client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("请求 %s 失败: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// 保存「当前生效模型」的编辑 → 服务端应实时热切换（Provider / BaseURL / Key / MaxTokens），
// 不需要再点「应用」。
func TestSaveActiveModelHotReloads(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	deps.cfg.LLM.Model = "glm-test"
	deps.cfg.LLM.APIKey = "sk-old"
	deps.cfg.LLM.MaxTokens = 8192

	client := withAuthClient(t, ts)
	resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id":         "glm-test",
		"name":       "GLM 测试",
		"base_url":   "https://api.example.com/v1",
		"protocol":   "custom",
		"key_source": "plain",
		"key_value":  "sk-new",
		"ctx_out":    131072,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存当前生效模型期望 200，实际 %d", resp.StatusCode)
	}

	// 请求带历史 protocol=custom：入库/热切换时服务端应归一化为 openai。
	if deps.cfg.LLM.Provider != "openai" {
		t.Errorf("实时切换后 Provider 期望 openai（custom 已合并），实际 %s", deps.cfg.LLM.Provider)
	}
	if deps.cfg.LLM.BaseURL != "https://api.example.com/v1" {
		t.Errorf("实时切换后 BaseURL 未更新，实际 %s", deps.cfg.LLM.BaseURL)
	}
	if deps.cfg.LLM.APIKey != "sk-new" {
		t.Errorf("实时切换后 APIKey 期望 sk-new，实际 %s", deps.cfg.LLM.APIKey)
	}
	if want := 131072; deps.cfg.LLM.MaxTokens != want {
		t.Errorf("实时切换后 MaxTokens 期望 %d（ctx_out 直填 tokens），实际 %d", want, deps.cfg.LLM.MaxTokens)
	}
	if deps.cfg.LLM.DisplayName != "GLM 测试" {
		t.Errorf("实时切换后 DisplayName 未更新，实际 %q", deps.cfg.LLM.DisplayName)
	}
}

// 保存非当前生效模型：只入库，不影响运行配置。
func TestSaveInactiveModelDoesNotTouchActiveConfig(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	deps.cfg.LLM.Model = "glm-active"
	deps.cfg.LLM.MaxTokens = 8192
	before := deps.cfg.LLM.BaseURL

	client := withAuthClient(t, ts)
	resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id":       "glm-other",
		"protocol": "openai",
		"base_url": "https://other.example.com/v1",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存非当前模型期望 200，实际 %d", resp.StatusCode)
	}

	if deps.cfg.LLM.Model != "glm-active" {
		t.Errorf("运行模型不应被切换，实际 %s", deps.cfg.LLM.Model)
	}
	if deps.cfg.LLM.BaseURL != before {
		t.Errorf("运行 BaseURL 不应被改动，实际 %s", deps.cfg.LLM.BaseURL)
	}
}

// 编辑已有模型但未重新填写密钥：库里已存的 key 必须原样保留。
// 前端拿到的列表是脱敏视图（明文框为空），「留空」的语义是保持而不是清空；
// 历史事故：编辑改个名字后点保存，密钥被空值覆盖，列表从「密钥✓」变成「无密钥」。
func TestSaveExistingModelKeepsStoredKey(t *testing.T) {
	cfgDir := t.TempDir()
	deps := newTestDepsAt(t, cfgDir)
	srv := deps.newServer()
	if err := srv.ModelStore().Upsert(config.ModelEntry{
		ID: "glm-keep", Name: "旧名字", Protocol: "openai",
		KeySource: "plain", KeyValue: "sk-keep",
	}); err != nil {
		t.Fatalf("预置条目失败: %v", err)
	}
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	// 1) 请求完全不带 key_value（新前端的编辑路径：明文框为空即不提交该字段）。
	resp := postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "glm-keep", "name": "新名字", "protocol": "openai",
		"base_url":   "https://api.example.com/v1",
		"key_source": "plain",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("不带 key_value 保存期望 200，实际 %d", resp.StatusCode)
	}
	m, ok := srv.ModelStore().Find("glm-keep")
	if !ok {
		t.Fatal("条目不应消失")
	}
	if m.KeyValue != "sk-keep" {
		t.Errorf("编辑不应清空已存密钥，实际 %q", m.KeyValue)
	}
	if m.Name != "新名字" || m.BaseURL != "https://api.example.com/v1" {
		t.Errorf("其余字段应正常更新，实际 %+v", m)
	}

	// 2) 请求显式带空 key_value（环境变量模式的编辑路径）：同样保持旧值。
	resp = postJSON(t, client, ts.URL+"/api/models/save", map[string]any{
		"id": "glm-keep", "name": "再改一次", "protocol": "openai",
		"key_source": "env", "key_name": "MY_KEY", "key_value": "",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("空 key_value 保存期望 200，实际 %d", resp.StatusCode)
	}
	m, _ = srv.ModelStore().Find("glm-keep")
	if m.KeyValue != "sk-keep" {
		t.Errorf("空 key_value 不应清空已存密钥，实际 %q", m.KeyValue)
	}
	if m.KeyName != "MY_KEY" {
		t.Errorf("key_name 应更新，实际 %q", m.KeyName)
	}
}
