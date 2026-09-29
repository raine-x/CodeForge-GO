// todo.go 提供 todo_write 工具：模型向会话提交完整任务清单（Claude TodoWrite 语义，
// 每次传整表，缺项即删除）。落库与 Agent 缓存由 TodoStore 回调（agent.Agent 实现）
// 完成，工具本身保持无状态（与 save_memory 相同的「工具→接口回调」结构）。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// TodoStore 是 todo_write 工具依赖的会话清单接口（agent.Agent 实现）。
type TodoStore interface {
	// SaveTodos 整体替换会话清单并更新 Agent 缓存（供 systemPrompt 注入）。
	SaveTodos(sessionID string, todos []store.TodoRow) error
}

// TodoTool 是任务清单写入工具。
type TodoTool struct{ store TodoStore }

// NewTodoTool 构造 todo_write 工具。
func NewTodoTool(store TodoStore) *TodoTool { return &TodoTool{store: store} }

// Metadata 声明副作用等级。
//
// run_command 的等级是 External 而非 Write：它能起进程、能碰网络、
// 写工作区之外的任何路径。用 Write 描述会低估它。
func (t *TodoTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectWrite}
}

// Name 实现 tools.Tool。
func (t *TodoTool) Name() string { return "todo_write" }

// Description 实现 tools.Tool。
func (t *TodoTool) Description() string {
	return "为当前会话维护一份结构化任务清单（提交时给出**完整**清单，未列出的旧项会被移除，故需包含以往未完成项）。" +
		"何时调用：任务有多个步骤、需要跨多轮跟进时，**动手前先规划** —— 把计划拆成一条条可勾选的步骤并提交；随后每完成一项就把该项状态改为 completed，调整计划时重新提交完整清单。" +
		"何时不调用：单步/纯聊天任务（不值得建清单）；收尾前把所有项一次性标成 completed（应随进度逐步更新，而不是最后补）。" +
		"status 取值：pending（待办）、in_progress（进行中，当前正在做的项）、completed（已完成）、cancelled（取消）。请保持清单与真实进度一致。"
}

// InputSchema 实现 tools.Tool。
func (t *TodoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "完整任务清单（每次整体提交）",
      "items": {
        "type": "object",
        "properties": {
          "content": {"type": "string", "description": "任务内容，一句话明确可勾选"},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"]},
          "priority": {"type": "integer", "description": "0=普通 1=优先"}
        },
        "required": ["content"]
      }
    }
  },
  "required": ["todos"]
}`)
}

// todoItem 是模型传入的单条任务。
type todoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority int    `json:"priority"`
}

var todoStatuses = map[string]bool{
	"pending":     true,
	"in_progress": true,
	"completed":   true,
	"cancelled":   true,
}

// Execute 实现 tools.Tool。
func (t *TodoTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	var p struct {
		Todos []todoItem `json:"todos"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if len(p.Todos) == 0 {
		return tools.Err("todos 不能为空（需要至少一条任务）"), nil
	}
	// 拒绝空字符串任务与非法状态：坏数据会直接污染清单与提示词。
	rows := make([]store.TodoRow, 0, len(p.Todos))
	for i, it := range p.Todos {
		content := strings.TrimSpace(it.Content)
		if content == "" {
			return tools.Err("第 %d 条任务内容为空", i+1), nil
		}
		if it.Status == "" {
			it.Status = "pending"
		}
		if !todoStatuses[it.Status] {
			return tools.Err("无效状态 %q（应为 pending/in_progress/completed/cancelled）", it.Status), nil
		}
		rows = append(rows, store.TodoRow{Content: content, Status: it.Status, Priority: it.Priority, Sort: i})
	}

	// 取出当前会话 ID：todo_write 只在主循环内有效（会话域由 Agent 注入）。
	sc, ok := tools.SessionFrom(ctx)
	if !ok {
		return tools.Err("todo_write 只能在会话运行中调用"), nil
	}
	if err := t.store.SaveTodos(sc.SessionID, rows); err != nil {
		return tools.Err("保存任务清单失败: %v", err), nil
	}
	// 通知 WS 层推送最新清单（Agent 缓存已在 SaveTodos 内更新）。
	if sink, ok := tools.TodoSinkFrom(ctx); ok {
		sink(sc.SessionID)
	}
	return tools.Ok(fmt.Sprintf("已更新任务清单（%d 条）。", len(rows))), nil
}

// RegisterTodo 注册任务清单工具。
func RegisterTodo(reg *tools.Registry, store TodoStore) {
	reg.Register(NewTodoTool(store))
}
