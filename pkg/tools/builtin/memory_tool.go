// memory_tool.go 提供 save_memory 工具：用户明确要求记住某内容时由模型调用。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"codeforge/pkg/tools"
)

// MemoryStore 是 save_memory 工具依赖的保存接口（agent.Agent 实现）。
type MemoryStore interface {
	AddMemory(content string) (int64, error)
}

// MemoryTool 是用户记忆保存工具。
type MemoryTool struct{ store MemoryStore }

// NewMemoryTool 构造 save_memory 工具。
func NewMemoryTool(store MemoryStore) *MemoryTool { return &MemoryTool{store: store} }

// Name 实现 tools.Tool。
func (t *MemoryTool) Name() string { return "save_memory" }

// Description 实现 tools.Tool。
func (t *MemoryTool) Description() string {
	return "把用户明确要求记住的内容保存为长期记忆（此后每轮对话自动生效）。当用户说「记住 X」「以后都 X」这类指令时调用，content 为要记住的简洁事实或偏好；普通对话不要调用。"
}

// InputSchema 实现 tools.Tool。
func (t *MemoryTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("content", "要记住的内容（一句话，简洁明确）", true).
		Build()
}

// Execute 实现 tools.Tool。
func (t *MemoryTool) Execute(_ context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	var p struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if p.Content == "" {
		return tools.Err("content 不能为空"), nil
	}
	id, err := t.store.AddMemory(p.Content)
	if err != nil {
		return tools.Err("保存记忆失败: %v", err), nil
	}
	return tools.Ok(fmt.Sprintf("已记住（记忆 #%d），后续对话会自动遵循。", id)), nil
}

// RegisterMemory 注册记忆工具。
func RegisterMemory(reg *tools.Registry, store MemoryStore) {
	reg.Register(NewMemoryTool(store))
}
