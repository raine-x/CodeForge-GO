// Package plugins 实现可扩展插件引擎，支持 Native / MCP-Stdio / HTTP / WASM 四种驱动。
package plugins

import (
	"context"
	"encoding/json"
	"fmt"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// Driver 是插件驱动的统一接口。
type Driver interface {
	// Tools 返回该插件暴露的全部工具。
	Tools(ctx context.Context) ([]tools.Tool, error)
	// Close 释放驱动持有的资源。
	Close() error
}

// RemoteTool 将插件能力适配为本地 tools.Tool。
type RemoteTool struct {
	plugin   string
	name     string
	desc     string
	schema   json.RawMessage
	invokeFn func(ctx context.Context, name string, args json.RawMessage) (*tools.ToolResult, error)
}

// Name 实现 tools.Tool。
func (t *RemoteTool) Name() string { return t.name }

// Description 实现 tools.Tool。
func (t *RemoteTool) Description() string { return t.desc }

// InputSchema 实现 tools.Tool。
func (t *RemoteTool) InputSchema() json.RawMessage { return t.schema }

// Execute 实现 tools.Tool。
func (t *RemoteTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	return t.invokeFn(ctx, t.name, args)
}

// Plugin 返回该工具所属的插件名。
func (t *RemoteTool) Plugin() string { return t.plugin }

// buildDriver 按配置构造对应驱动。
func buildDriver(cfg config.PluginConfig) (Driver, error) {
	switch cfg.Type {
	case "mcp":
		return newMCPDriver(cfg)
	case "http":
		return newHTTPDriver(cfg)
	case "wasm":
		return newWASMDriver(cfg)
	case "native":
		return nil, fmt.Errorf("native 插件应直接编译进程序，无需动态加载")
	default:
		return nil, fmt.Errorf("不支持的插件类型: %q", cfg.Type)
	}
}

// genericSchema 为无法自描述的工具提供通用入参 Schema。
func genericSchema() json.RawMessage {
	return tools.NewSchema().
		Str("arguments", "工具参数（自由 JSON 对象，按插件约定传入）", false).
		Build()
}

// qualify 为插件工具名加上插件前缀，避免命名冲突。
func qualify(plugin, tool string) string {
	if plugin == "" {
		return tool
	}
	return plugin + "." + tool
}
