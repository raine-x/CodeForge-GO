// driver_mcphttp.go 实现 Streamable HTTP 传输的 MCP 客户端驱动。
//
// 与 stdio 驱动（driver_mcp.go）共享 JSON-RPC 2.0 语法：initialize 握手、
// tools/list 发现、tools/call 调用；区别仅在传输层 —— 每条消息是一次
// HTTP POST，响应可为：
//   - 一次性 JSON（现代服务器，如 Parallel.ai 的 streamable HTTP）；
//   - SSE 流（HTTP with SSE 传输的 POST 回复）。
//
// 本驱动两种都解析，兼容面更广。
package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// mcpHTTPDriver 通过 Streamable HTTP POST 与远程 MCP 服务通信。
type mcpHTTPDriver struct {
	cfg    config.PluginConfig
	client *http.Client

	mu      sync.Mutex
	nextID  int
	timeout time.Duration
}

// newMCPHTTPDriver 构造远程 MCP 驱动（type=mcp-http）。
func newMCPHTTPDriver(cfg config.PluginConfig) (*mcpHTTPDriver, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, fmt.Errorf("MCP-HTTP 插件 %s 缺少 endpoint", cfg.Name)
	}
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	return &mcpHTTPDriver{
		cfg:     cfg,
		client:  &http.Client{Timeout: 90 * time.Second, Transport: transport},
		nextID:  1,
		timeout: 90 * time.Second,
	}, nil
}

// initialize 完成 Streamable HTTP 握手。
func (d *mcpHTTPDriver) initialize(ctx context.Context) error {
	_, err := d.request(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codeforge", "version": "1.0.0"},
	})
	return err
}

// request 发送一条 JSON-RPC 请求并等待响应（单飞行串行）。
func (d *mcpHTTPDriver) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id := d.nextID
	d.nextID++

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}

	rctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, d.cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Streamable HTTP：服务器按 Accept 偏好选择一次性 JSON 或 SSE。
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("MCP-HTTP 请求 %s 失败: %w", method, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, fmt.Errorf("MCP-HTTP 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		// SSE 里的 data 行装的仍是完整的 JSON-RPC 信封，必须和一次性 JSON 走同一套
		// 解包（校验 id、抛 error、取出 result）。否则两条传输路径返回的东西不一致：
		// JSON 路径给 result，SSE 路径给整个信封 → 上层按 result 结构解析会得到空结果
		// （实测 Exa 走 SSE，表现为「握手成功、零报错、但 0 个工具、调用返回空内容」）。
		msg, err := d.readSSEResponse(resp.Body)
		if err != nil {
			return nil, err
		}
		return d.parseJSONResponse(msg, id)
	}
	// 一次性 JSON
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return d.parseJSONResponse(body, id)
}

// parseJSONResponse 校验一次响应是否匹配请求 id，return result 或错误。
func (d *mcpHTTPDriver) parseJSONResponse(body []byte, id int) (json.RawMessage, error) {
	var resp struct {
		ID     *int            `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("MCP-HTTP 空响应")
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("MCP-HTTP 响应解析失败: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP 错误 %d: %s", resp.Error.Code, resp.Error.Message)
	}
	if resp.ID != nil && *resp.ID != id {
		return nil, fmt.Errorf("MCP-HTTP 响应 id 不匹配（期望 %d 实际 %d）", id, *resp.ID)
	}
	return resp.Result, nil
}

// readSSEResponse 解析 SSE 流中的 message 事件（HTTP with SSE 传输下 POST 的回复）。
// 返回的是 data 行的原始内容 —— 即完整的 JSON-RPC 信封，由调用方（request）统一解包。
func (d *mcpHTTPDriver) readSSEResponse(r io.Reader) (json.RawMessage, error) {
	reader := bufio.NewReaderSize(r, 8<<20)
	var dataBuf strings.Builder
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			dataBuf.WriteString(strings.TrimSpace(line[len("data:"):]))
		}
		if err != nil {
			if err == io.EOF && dataBuf.Len() > 0 {
				break
			}
			if err != io.EOF {
				return nil, fmt.Errorf("MCP-HTTP SSE 读取失败: %w", err)
			}
			return nil, fmt.Errorf("MCP-HTTP SSE 无 message 事件")
		}
		if line == "" && dataBuf.Len() > 0 {
			break
		}
	}
	return []byte(dataBuf.String()), nil
}

// Tools 实现 Driver：initialize 后调用 tools/list 发现工具。
func (d *mcpHTTPDriver) Tools(ctx context.Context) ([]tools.Tool, error) {
	if err := d.initialize(ctx); err != nil {
		return nil, fmt.Errorf("MCP-HTTP 初始化失败: %w", err)
	}
	raw, err := d.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("解析 tools/list 结果失败: %w", err)
	}

	out := make([]tools.Tool, 0, len(res.Tools))
	for _, t := range res.Tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = genericSchema()
		}
		out = append(out, &RemoteTool{
			name:       t.Name,
			remoteName: t.Name,
			desc:       t.Description,
			schema:     schema,
			invokeFn: func(ctx context.Context, callName string, args json.RawMessage) (*tools.ToolResult, error) {
				return d.callTool(ctx, callName, args)
			},
		})
	}
	return out, nil
}

// callTool 调用远端工具（tools/call），内容拼 text 类型的 Content 到结果。
func (d *mcpHTTPDriver) callTool(ctx context.Context, name string, args json.RawMessage) (*tools.ToolResult, error) {
	arguments := any(map[string]any{})
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			arguments = map[string]any{}
		}
	}
	raw, err := d.request(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("解析 tools/call 结果失败: %w", err)
	}

	var sb strings.Builder
	for _, c := range res.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	out := &tools.ToolResult{Success: !res.IsError, Data: sb.String()}
	if res.IsError {
		out.Error = sb.String()
	}
	return out, nil
}

// Close 实现 Driver：无常驻连接，仅置空。
func (d *mcpHTTPDriver) Close() error { return nil }
