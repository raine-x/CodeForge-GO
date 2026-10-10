package builtin

import (
	"context"
	"encoding/json"

	"codeforge/pkg/errs"
	"codeforge/pkg/tools"
)

// ToolOutTool 读回被外置的工具结果。
//
// 为什么需要它：超过阈值的大结果不再被硬截断（见 pkg/tools/spill.go），
// 而是完整落盘、上下文里只留预览。要看细节就得能取回来 —— 没有这个工具，
// 外置就等于换了一种丢法。
//
// 刻意**不用** read_file 走文件围栏那条路：外置文件在 ~/.codeforge/toolout/ 下，
// 是进程级 scratch，不在工作区内。让 read_file 能读它等于给文件围栏开一个洞
// （要么把外置目录塞进 allow_outside_workspace，要么放宽围栏），两者的安全面
// 都比「一个只能读 codeforge 自己写的文件」的专用工具大得多。
type ToolOutTool struct {
	store *tools.ToolOutStore
}

// NewToolOutTool 构造读回工具。store 为 nil 时注册它没有意义，
// 但保留构造以免调用方各自判断。
func NewToolOutTool(store *tools.ToolOutStore) *ToolOutTool {
	return &ToolOutTool{store: store}
}

// Name 实现 tools.Tool。
func (t *ToolOutTool) Name() string { return "tool_result_read" }

// Description 实现 tools.Tool。
func (t *ToolOutTool) Description() string {
	return "分段读回一个被外置的工具输出。当上一次工具结果显示「【输出已外置】」时，" +
		"用它在结果里给出的 id 继续读后面的内容；不要凭预览猜测被省略的部分。" +
		"id 形如 tr_a1b2c3，只能读本会话自己的外置输出。"
}

// InputSchema 实现 tools.Tool。
func (t *ToolOutTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("id", "外置输出 id（形如 tr_a1b2c3，取自结果里的「【输出已外置】」提示）", true).
		Int("start_line", "起始行号，1-based，可选（默认第 1 行）", false).
		Int("end_line", "结束行号，含端点，可选（默认到末尾）", false).
		Build()
}

// Metadata 声明副作用等级：纯读，不碰工作区，不发起外部请求。
func (t *ToolOutTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}

// Execute 实现 tools.Tool。
//
// 会话 id 取自 ctx（模型无法指定），与外置时写入用的会话是同一个 ——
// 这就是「只能读本会话的外置输出」的全部实现，没有额外的权限判定。
func (t *ToolOutTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if t.store == nil || !t.store.Enabled() {
		return tools.Err("工具结果外置未启用，无法读回"), nil
	}
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return tools.Err("当前不在会话内，无法读回外置输出"), nil
	}
	var p struct {
		ID        string `json:"id"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	out, err := t.store.Read(sc.SessionID, p.ID, p.StartLine, p.EndLine)
	if err != nil {
		// 这类是「用法/范围」错误（id 不存在、start_line 超界），不是系统故障，
		// FriendlyOr 会原样返回，读起来比套一层「执行工具失败」清楚。
		return tools.Err("%s", errs.FriendlyOr("读取外置输出", err)), nil
	}
	return tools.Ok(out), nil
}

// IsReadOnly 声明只读。
func (t *ToolOutTool) IsReadOnly() bool { return true }
