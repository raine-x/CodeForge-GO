package tools

import (
	"context"
	"encoding/json"
	"testing"
)

// stubTool 最小工具实现（供注册测试）。
type stubTool struct{ name string }

func (s stubTool) Name() string                 { return s.name }
func (s stubTool) Description() string          { return "stub " + s.name }
func (s stubTool) InputSchema() json.RawMessage { return []byte(`{"type":"object"}`) }
func (s stubTool) Execute(context.Context, json.RawMessage) (*ToolResult, error) {
	return nil, nil // 不会被调用
}

// 带点号的插件工具名清洗为上游合法格式，ResolveWire 能还原；
// 合法名（内置工具）不产生映射。
func TestWireNameSanitize(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool{"github_tools.create_issue"})
	r.Register(stubTool{"read_file"})

	defs := r.Definitions()
	if len(defs) != 2 {
		t.Fatalf("期望 2 个定义，实际 %d", len(defs))
	}
	found := map[string]string{}
	for _, d := range defs {
		found[d.Name] = d.Name
	}
	// 点号被清洗成下划线
	if _, ok := found["github_tools_create_issue"]; !ok {
		t.Errorf("wire 名应为 github_tools_create_issue，实际 %v", found)
	}
	// 合法名原样
	if _, ok := found["read_file"]; !ok {
		t.Errorf("合法名应保持 read_file，实际 %v", found)
	}

	// 还原
	if got := r.ResolveWire("github_tools_create_issue"); got != "github_tools.create_issue" {
		t.Errorf("ResolveWire 还原失败，实际 %q", got)
	}
	if got := r.ResolveWire("read_file"); got != "read_file" {
		t.Errorf("合法名应原样返回，实际 %q", got)
	}
}

// 清洗后撞名（a.b 与 a_b 同时存在）→ 追加序号保证唯一，还原各自正确。
func TestWireNameCollision(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool{"a.b"})
	r.Register(stubTool{"a_b"})

	defs := r.Definitions()
	names := map[string]bool{}
	for _, d := range defs {
		names[d.Name] = true
	}
	if len(defs) != 2 || !names["a_b"] || !names["a_b_2"] {
		t.Fatalf("撞名应清洗为 a_b 与 a_b_2，实际 %v", names)
	}
	if got := r.ResolveWire("a_b"); got != "a.b" && got != "a_b" {
		// 排序后 a.b 先注册 → a_b；a_b 后注册 → a_b_2。两者必须可区分还原。
		t.Errorf("ResolveWire(a_b) = %q", got)
	}
	if r.ResolveWire("a_b") == r.ResolveWire("a_b_2") {
		t.Errorf("两个撞名工具还原后不应相同")
	}
}

// 注销后重建：遗留映射被清理，不指向已注销工具。
func TestWireNameAfterUnregister(t *testing.T) {
	r := NewRegistry()
	r.Register(stubTool{"x.y"})
	_ = r.Definitions()
	r.Unregister("x.y")
	_ = r.Definitions()
	if got := r.ResolveWire("x_y"); got != "x_y" {
		// 注销后 x_y 不应再映射回 x.y
		t.Errorf("注销后遗留映射未清理：ResolveWire(x_y) = %q", got)
	}
}
