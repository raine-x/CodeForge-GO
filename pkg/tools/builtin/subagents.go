package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codeforge/pkg/tools"
)

// SubagentsTool 是多智能体委派工具。
// 调度规则、最大并发数和代码范围互斥由 runner 统一执行；模型只负责提供任务分解。
type SubagentsTool struct {
	runner tools.SubagentRunner
}

// NewSubagentsTool 构造多智能体工具。
func NewSubagentsTool(runner tools.SubagentRunner) *SubagentsTool {
	return &SubagentsTool{runner: runner}
}

func (t *SubagentsTool) Name() string { return "delegate_subagents" }

func (t *SubagentsTool) Description() string {
	return "将多个互不重叠的代码探索或实现子任务并行委派给最多 5 个子智能体；仅当任务确实可拆分且子任务不操作相同代码范围时使用。explore 子任务只能只读探索，implement 必须声明不重叠的 paths。"
}

func (t *SubagentsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"object","properties":{"id":{"type":"string","description":"短任务标识"},"prompt":{"type":"string","description":"子任务的完整目标与验收要求"},"mode":{"type":"string","enum":["explore","implement"],"description":"explore=只读代码探索；implement=在声明范围内实现"},"paths":{"type":"array","items":{"type":"string"},"description":"任务允许关注/修改的相对路径；不同任务之间不得重叠"}},"required":["id","prompt","mode"]}}},"required":["tasks"]}`)
}

func (t *SubagentsTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if t.runner == nil {
		return tools.Err("多智能体调度器未初始化"), nil
	}
	var in struct {
		Tasks []tools.SubagentTask `json:"tasks"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Err("tasks 参数解析失败：%v", err), nil
	}
	for i := range in.Tasks {
		in.Tasks[i].ID = strings.TrimSpace(in.Tasks[i].ID)
		in.Tasks[i].Prompt = strings.TrimSpace(in.Tasks[i].Prompt)
		in.Tasks[i].Mode = strings.ToLower(strings.TrimSpace(in.Tasks[i].Mode))
	}
	results, err := t.runner.RunSubagents(ctx, in.Tasks)
	if err != nil {
		return tools.Err("多智能体调度失败：%v", err), nil
	}
	return tools.Ok(map[string]any{
		"message": fmt.Sprintf("已完成 %d 个子任务；请主智能体综合结果，不要重复执行已完成的范围", len(results)),
		"results": results,
	}), nil
}

// RegisterSubagents 注册内置多智能体工具。
func RegisterSubagents(r *tools.Registry, runner tools.SubagentRunner) {
	r.Register(NewSubagentsTool(runner))
}
