package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

// authedClient 走首页拿到鉴权 Cookie 后复用。
func authedClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取 Cookie 失败: %v", err)
	}
	_ = resp.Body.Close()
	return client
}

func postRetry(t *testing.T, client *http.Client, url string, body any) (int, map[string]any) {
	t.Helper()
	resp, err := client.Post(url, "application/json", mustJSON(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestRetrySettingsSaveAndValidate 重试设置：合法值保存后 GET 能读回、热生效；
// 非法模式 / 越界次数直接拒绝，不污染配置。
func TestRetrySettingsSaveAndValidate(t *testing.T) {
	ts := newTestServer(t)
	client := authedClient(t, ts)

	// 1) 保存合法配置：退避模式 + 8 次 + 3 秒基准
	status, out := postRetry(t, client, ts.URL+"/api/config", map[string]any{
		"retry_max_attempts": 8,
		"retry_mode":         "fixed",
		"retry_interval_sec": 3,
	})
	if status != http.StatusOK {
		t.Fatalf("保存合法重试配置期望 200，实际 %d: %v", status, out)
	}

	resp, err := client.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("读取配置失败: %v", err)
	}
	defer resp.Body.Close()
	var cfg map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg["retry_max_attempts"] != float64(8) {
		t.Errorf("重试次数期望读回 8，实际 %v", cfg["retry_max_attempts"])
	}
	if cfg["retry_mode"] != "fixed" {
		t.Errorf("重试模式期望读回 fixed，实际 %v", cfg["retry_mode"])
	}
	if cfg["retry_interval_sec"] != float64(3) {
		t.Errorf("基础间隔期望读回 3 秒，实际 %v", cfg["retry_interval_sec"])
	}

	// 2) 非法模式拒绝
	status, out = postRetry(t, client, ts.URL+"/api/config", map[string]any{"retry_mode": "random"})
	if status != http.StatusBadRequest {
		t.Errorf("非法重试模式期望 400，实际 %d: %v", status, out)
	}

	// 3) 越界次数拒绝（上限 15）
	status, out = postRetry(t, client, ts.URL+"/api/config", map[string]any{"retry_max_attempts": 99})
	if status != http.StatusBadRequest {
		t.Errorf("越界重试次数期望 400，实际 %d: %v", status, out)
	}

	// 4) 上一步的拒绝不应改动已保存的值
	resp2, err := client.Get(ts.URL + "/api/config")
	if err != nil {
		t.Fatalf("再次读取配置失败: %v", err)
	}
	defer resp2.Body.Close()
	var cfg2 map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&cfg2)
	if cfg2["retry_max_attempts"] != float64(8) {
		t.Errorf("拒绝非法请求后重试次数应保持 8，实际 %v", cfg2["retry_max_attempts"])
	}
}

// mustJSON 编码请求体（测试专用，失败即终止）。
func mustJSON(v any) *strings.Reader {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return strings.NewReader(string(b))
}
