// memory.go 实现用户记忆：用户让 AI「记住」的内容存入 SQLite，
// 每轮注入 System Prompt；工作区隔离（每个工作区有独立的记忆集）。
package agent

import (
	"fmt"
	"strings"
	"time"

	"codeforge/pkg/store"
)

// MemorySection 生成注入 System Prompt 的「用户记忆」段落。
// memories 为空返回空串（不占 token）。
func MemorySection(memories []store.MemoryRow) string {
	if len(memories) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## 用户记忆\n以下内容是用户明确要求你记住的偏好/事实，回答时始终遵守：\n")
	for _, m := range memories {
		fmt.Fprintf(&sb, "- %s\n", strings.TrimSpace(m.Content))
	}
	return sb.String()
}

// loadMemories 读取工作区全部记忆（新→旧），供注入。
func (a *Agent) loadMemories() []store.MemoryRow {
	if a.memoryStore == nil {
		return nil
	}
	list, err := a.memoryStore.ListMemories(a.WorkDir())
	if err != nil {
		return nil
	}
	return list
}

// AddMemory 由工具调用链路新增一条用户记忆。
func (a *Agent) AddMemory(content string) (int64, error) {
	if a.memoryStore == nil {
		return 0, fmt.Errorf("记忆存储未初始化")
	}
	return a.memoryStore.AddMemory(a.WorkDir(), strings.TrimSpace(content))
}

// ListMemories 返回当前工作区全部记忆（REST 用）。
func (a *Agent) ListMemories() []store.MemoryRow { return a.loadMemories() }

// UpdateMemory 更新一条记忆（REST 用）。
func (a *Agent) UpdateMemory(id int64, content string) error {
	if a.memoryStore == nil {
		return fmt.Errorf("记忆存储未初始化")
	}
	return a.memoryStore.UpdateMemory(id, strings.TrimSpace(content))
}

// DeleteMemory 删除一条记忆（REST 用）。
func (a *Agent) DeleteMemory(id int64) error {
	if a.memoryStore == nil {
		return fmt.Errorf("记忆存储未初始化")
	}
	return a.memoryStore.DeleteMemory(id)
}

// SetMemoryStore 注入记忆存储（与 History 共用同一个 SQLite Store）。
func (a *Agent) SetMemoryStore(st *store.Store) { a.memoryStore = st }

// memoryNow 仅供测试的时间占位。
var memoryNow = time.Now
