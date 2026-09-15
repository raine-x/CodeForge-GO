// todos.go 是 Agent 侧任务清单的读写与提示词注入。
//
// 数据流：todo_write（tools/builtin/todo.go）→ SaveTodos 落库 → runLoop 每步
// 经 TodoSection 注入 System Prompt（清单随会话持久化，摘要压缩后仍在）；
// 前端实时更新走 WS 层注入的 tools.TodoSink。
package agent

import (
	"fmt"
	"strings"

	"codeforge/pkg/store"
)

// SaveTodos 整体替换会话任务清单并返回错误（tools.TodoStore 接口实现）。
func (a *Agent) SaveTodos(sessionID string, todos []store.TodoRow) error {
	return a.history.ReplaceTodos(sessionID, todos)
}

// Todos 返回会话的任务清单（无清单返回空数组）。
func (a *Agent) Todos(sessionID string) []store.TodoRow {
	return a.history.Todos(sessionID)
}

var todoStatusIcon = map[string]string{
	"pending":     "○",
	"in_progress": "◐",
	"completed":   "✓",
	"cancelled":   "✕",
}

// TodoSection 生成 System Prompt 里的任务清单段；无清单（或会话不可取）返回空串。
// 格式稳定：每行 "状态图标 内容"，按 sort 排；模型据此知道当前所有待办与进度。
func (a *Agent) TodoSection(sessionID string) string {
	todos := a.history.Todos(sessionID)
	if len(todos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 当前任务清单\n")
	b.WriteString("以下是本会话尚未完成 / 正在进行的任务（完成一项就更新状态）：\n")
	for _, t := range todos {
		icon := todoStatusIcon[t.Status]
		if icon == "" {
			icon = "○"
		}
		fmt.Fprintf(&b, "- %s %s\n", icon, t.Content)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// systemPromptFor 生成会话维度的 System Prompt：在 systemPrompt 基础上追加任务清单。
func (a *Agent) systemPromptFor(sess *Session) string {
	base := a.systemPrompt()
	if sess == nil {
		return base
	}
	if sec := a.TodoSection(sess.ID); sec != "" {
		return base + "\n\n" + sec
	}
	return base
}