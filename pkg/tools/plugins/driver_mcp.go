package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"codeforge/config"
	"codeforge/pkg/tools"
)

const mcpProtocolVersion = "2024-11-05"

// mcpDriver 通过 stdio 上的 JSON-RPC 2.0 与外部 MCP 进程通信。
type mcpDriver struct {
	cfg config.PluginConfig

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	readErr chan error

	mu      sync.Mutex
	nextID  int
	timeout time.Duration
}

// newMCPDriver 启动插件进程并完成 initialize 握手。
func newMCPDriver(cfg config.PluginConfig) (*mcpDriver, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("MCP 插件 %s 缺少 command", cfg.Name)
	}
	d := &mcpDriver{
		cfg:     cfg,
		lines:   make(chan string, 128),
		readErr: make(chan error, 1),
		nextID:  1,
		timeout: 30 * time.Second,
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Env = append(os.Environ(), envPairs(cfg.Env)...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 MCP 进程失败: %w", err)
	}
	d.cmd = cmd
	d.stdin = stdin
	go d.readLoop(stdout)

	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()
	if _, err := d.request(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codeforge", "version": "1.0.0"},
	}); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("MCP initialize 失败: %w", err)
	}
	_ = d.notify("notifications/initialized", nil)
	return d, nil
}

// readLoop 持续读取子进程 stdout，按行投递。
func (d *mcpDriver) readLoop(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<20)
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(trimmed) != "" {
			d.lines <- trimmed
		}
		if err != nil {
			d.readErr <- err
			close(d.lines)
			return
		}
	}
}

// request 发送 JSON-RPC 请求并等待同 id 的响应（单飞行串行）。
func (d *mcpDriver) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	id := d.nextID
	d.nextID++

	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := d.write(msg); err != nil {
		return nil, err
	}

	timer := time.NewTimer(d.timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("MCP 请求 %s 超时", method)
		case <-d.readErr:
			return nil, fmt.Errorf("MCP 进程已退出")
		case line, ok := <-d.lines:
			if !ok {
				return nil, fmt.Errorf("MCP 进程输出已关闭")
			}
			var resp struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				continue
			}
			if resp.ID == nil || *resp.ID != id {
				continue // 忽略通知或其他响应
			}
			if resp.Error != nil {
				return nil, fmt.Errorf("MCP 错误 %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return resp.Result, nil
		}
	}
}

// notify 发送无需响应的通知。
func (d *mcpDriver) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.write(msg)
}

func (d *mcpDriver) write(v any) error {
	if d.stdin == nil {
		return fmt.Errorf("MCP stdin 已关闭")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = d.stdin.Write(append(data, '\n'))
	return err
}

// Tools 实现 Driver：调用 tools/list 发现工具。
func (d *mcpDriver) Tools(ctx context.Context) ([]tools.Tool, error) {
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
			name:   t.Name,
			desc:   t.Description,
			schema: schema,
			invokeFn: func(ctx context.Context, callName string, args json.RawMessage) (*tools.ToolResult, error) {
				return d.callTool(ctx, callName, args)
			},
		})
	}
	return out, nil
}

// callTool 调用远端工具（tools/call）。
func (d *mcpDriver) callTool(ctx context.Context, name string, args json.RawMessage) (*tools.ToolResult, error) {
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

// Close 实现 Driver：终止子进程。
func (d *mcpDriver) Close() error {
	if d.stdin != nil {
		_ = d.stdin.Close()
	}
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		_, _ = d.cmd.Process.Wait()
	}
	return nil
}

// envPairs 将 map 展开为 KEY=VALUE 列表，并支持 ${VAR} 环境变量插值。
func envPairs(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+os.ExpandEnv(v))
	}
	return out
}
