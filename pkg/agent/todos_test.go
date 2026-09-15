package agent

import (
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/store"
)

// newCtxAgentWithStore 构造带真实 SQLite 存储的 Agent（SaveTodos/TodoSection 依赖 History）。
func newCtxAgentWithStore(t *testing.T) *Agent {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Agent{history: NewHistory(st)}
}

// mustRows 把 [2]string{content, status} 转成 store.TodoRow。
func mustRows(t *testing.T, in [][2]string) []store.TodoRow {
	t.Helper()
	out := make([]store.TodoRow, 0, len(in))
	for i, p := range in {
		out = append(out, store.TodoRow{Content: p[0], Status: p[1], Sort: i})
	}
	return out
}

// 任务清单的提示词注入：清单段必须进 System Prompt（随会话持久化，压缩后仍在），
// 且格式稳定（图标 + 内容），方便前端与模型对齐。
func TestTodoSectionInjectsIntoSysPrompt(t *testing.T) {
	ag := newCtxAgentWithStore(t)
	// 用 History.Create 建真实会话（session_todos 有外键约束，先有父记录才能写入）。
	sess, err := ag.history.Create("ws", "清单测试")
	if err != nil {
		t.Fatal(err)
	}

	// 无清单 → 不注入（检查小节标题，而非「任务清单」字样：基础提示词的工具
	// 对照表本身含「更新任务清单」，若按字样判断会误报）
	if sec := ag.TodoSection(sess.ID); sec != "" {
		t.Errorf("无清单时应为空，实际: %q", sec)
	}
	if p := ag.systemPromptFor(sess); strings.Contains(p, "## 当前任务清单") {
		t.Errorf("无清单时提示词不应含清单段: %q", p)
	}

	// 写入 3 条不同状态
	if err := ag.SaveTodos(sess.ID, mustRows(t, [][2]string{
		{"读文档", "pending"},
		{"改代码", "in_progress"},
		{"跑测试", "completed"},
	})); err != nil {
		t.Fatal(err)
	}

	sec := ag.TodoSection(sess.ID)
	if !strings.Contains(sec, "○ 读文档") || !strings.Contains(sec, "◐ 改代码") || !strings.Contains(sec, "✓ 跑测试") {
		t.Errorf("清单段图标/内容不符: %s", sec)
	}
	if !strings.Contains(ag.systemPromptFor(sess), "## 当前任务清单") {
		t.Errorf("systemPromptFor 应注入清单段")
	}
}
