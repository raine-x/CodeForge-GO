package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// httpDriver 通过 HTTP Webhook 调用远端工具服务。
type httpDriver struct {
	cfg    config.PluginConfig
	client *http.Client
}

// newHTTPDriver 构造 HTTP 插件驱动。
func newHTTPDriver(cfg config.PluginConfig) (*httpDriver, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("HTTP 插件 %s 缺少 endpoint", cfg.Name)
	}
	return &httpDriver{
		cfg:    cfg,
		client: &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// Tools 实现 Driver：HTTP 插件默认暴露单个以插件名命名的工具。
func (d *httpDriver) Tools(_ context.Context) ([]tools.Tool, error) {
	desc := d.cfg.Description
	if strings.TrimSpace(desc) == "" {
		desc = "HTTP Webhook 插件：" + d.cfg.Name
	}
	return []tools.Tool{&RemoteTool{
		name:     d.cfg.Name,
		desc:     desc,
		schema:   genericSchema(),
		invokeFn: d.invoke,
	}}, nil
}

// invoke 向 endpoint POST {name, arguments}，期望返回 {success, data, error}。
func (d *httpDriver) invoke(ctx context.Context, name string, args json.RawMessage) (*tools.ToolResult, error) {
	arguments := any(map[string]any{})
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			arguments = map[string]any{}
		}
	}
	payload, err := json.Marshal(map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return tools.Err("HTTP 插件返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body))), nil
	}

	var out struct {
		Success bool   `json:"success"`
		Data    any    `json:"data"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return tools.Ok(string(body)), nil
	}
	res := &tools.ToolResult{Success: out.Success, Data: out.Data}
	if !out.Success {
		res.Error = out.Error
	}
	return res, nil
}

// Close 实现 Driver。
func (d *httpDriver) Close() error { return nil }
