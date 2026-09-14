package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"codeforge/pkg/tools"
)

// DefaultSubagentMax 是未配置时的子智能体并发上限（与 agent.MaxSubagents 同源）。
const DefaultSubagentMax = 5

// SubagentsTool 是多智能体委派工具。
//
// 调度规则、并发上限和代码范围互斥由 runner 统一执行；模型只负责提供任务分解。
// 并发上限可在设置页调整，因此工具的 Description 与 InputSchema 都是**动态**的：
// 若描述里写死「最多 5 个」而上限已被调成 2，模型会按 5 个去拆分，然后被
// 调度器拒绝 —— 白跑一轮。这里让三者（描述 / schema / 校验）始终一致。
type SubagentsTool struct {
	runner tools.SubagentRunner

	maxMu sync.RWMutex
	max   int // 0 表示用 DefaultSubagentMax
}

// NewSubagentsTool 构造多智能体工具（默认并发上限）。
func NewSubagentsTool(runner tools.SubagentRunner) *SubagentsTool {
	return &SubagentsTool{runner: runner}
}

// SetMaxConcurrent 更新并发上限（设置页热更新）。
// <=0 或超出硬上限的取值一律回落到 DefaultSubagentMax。
func (t *SubagentsTool) SetMaxConcurrent(n int) {
	if n <= 0 || n > DefaultSubagentMax {
		n = DefaultSubagentMax
	}
	t.maxMu.Lock()
	t.max = n
	t.maxMu.Unlock()
}

// limit 返回当前生效的并发上限。
func (t *SubagentsTool) limit() int {
	t.maxMu.RLock()
	defer t.maxMu.RUnlock()
	if t.max <= 0 || t.max > DefaultSubagentMax {
		return DefaultSubagentMax
	}
	return t.max
}

func (t *SubagentsTool) Name() string { return "delegate_subagents" }

func (t *SubagentsTool) Description() string {
	return fmt.Sprintf("将多个互不重叠的代码探索或实现子任务并行委派给最多 %d 个子智能体；"+
		"仅当任务确实可拆分且子任务不操作相同代码范围时使用。"+
		"explore 子任务只能只读探索，implement 必须声明不重叠的 paths。", t.limit())
}

func (t *SubagentsTool) InputSchema() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{"id":{"type":"string","description":"短任务标识"},"prompt":{"type":"string","description":"子任务的完整目标与验收要求"},"mode":{"type":"string","enum":["explore","implement"],"description":"explore=只读代码探索；implement=在声明范围内实现"},"paths":{"type":"array","items":{"type":"string"},"description":"任务允许关注/修改的相对路径；不同任务之间不得重叠"}},"required":["id","prompt","mode"]}}},"required":["tasks"]}`, t.limit()))
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

// RegisterSubagents 注册内置多智能体工具，并返回工具实例，
// 供调用方在设置变更时同步并发上限（SetMaxConcurrent）。
func RegisterSubagents(r *tools.Registry, runner tools.SubagentRunner) *SubagentsTool {
	tool := NewSubagentsTool(runner)
	r.Register(tool)
	return tool
}
