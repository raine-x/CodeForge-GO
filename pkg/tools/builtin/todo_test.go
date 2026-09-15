package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// stubTodoStore 记录落库内容，验证「工具→接口回调」路径。
type stubTodoStore struct {
	sess string
	rows []store.TodoRow
}

func (s *stubTodoStore) SaveTodos(sessionID string, rows []store.TodoRow) error {
	s.sess = sessionID
	s.rows = rows
	return nil
}

// execTodo 在会话域 + todo sink 就绪的环境里执行一次 todo_write。
func execTodo(t *testing.T, tool *TodoTool, store *stubTodoStore, args string) (*tools.ToolResult, int) {
	t.Helper()
	sinked := 0
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s-todo", Step: 2})
	ctx = tools.WithTodoSink(ctx, func(string) { sinked++ })
	res, err := tool.Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute 不应返回 error，实际 %v", err)
	}
	return res, sinked
}

func TestTodoWriteSuccessPersistsAndSinks(t *testing.T) {
	st := &stubTodoStore{}
	tool := NewTodoTool(st)
	res, sinked := execTodo(t, tool, st, `{"todos":[
		{"content":"调研方案"},
		{"content":"实现","status":"in_progress","priority":1},
		{"content":"收尾","status":"completed"}
	]}`)

	if !res.Success {
		t.Fatalf("合法输入应成功: %+v", res)
	}
	if st.sess != "s-todo" {
		t.Errorf("落库应归属会话 s-todo，实际 %q", st.sess)
	}
	if len(st.rows) != 3 {
		t.Fatalf("应落库 3 条，实际 %d", len(st.rows))
	}
	if st.rows[1].Status != "in_progress" || st.rows[2].Status != "completed" {
		t.Errorf("状态透传不对: %+v", st.rows)
	}
	if st.rows[1].Priority != 1 {
		t.Errorf("priority 透传不对: %+v", st.rows[1])
	}
	if st.rows[0].Sort != 0 || st.rows[2].Sort != 2 {
		t.Errorf("sort 应按输入顺序编号: %+v", st.rows)
	}
	if sinked != 1 {
		t.Errorf("清单更新应触发一次 sink，实际 %d", sinked)
	}
}

func TestTodoWriteRejectsBadInput(t *testing.T) {
	st := &stubTodoStore{}
	tool := NewTodoTool(st)

	cases := []struct {
		name string
		args string
	}{
		{"空清单", `{"todos":[]}`},
		{"空内容", `{"todos":[{"content":"  "}]}`},
		{"非法状态", `{"todos":[{"content":"x","status":"dangling"}]}`},
		{"非法 JSON", `{"todos":[`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, _ := execTodo(t, tool, st, c.args)
			if res.Success {
				t.Fatalf("非法输入不应成功: %+v", res)
			}
			if len(st.rows) != 0 {
				t.Errorf("非法输入不得触发落库: %+v", st.rows)
			}
		})
	}
}

func TestTodoWriteRequiresSessionScope(t *testing.T) {
	st := &stubTodoStore{}
	tool := NewTodoTool(st)
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"todos":[{"content":"x"}]}`))
	if res.Success {
		t.Fatal("无会话域时不应成功（todo 必须归属某会话）")
	}
	if len(st.rows) != 0 {
		t.Errorf("无会话域不应落库: %+v", st.rows)
	}
}

// 描述里必须带完整状态枚举，否则模型会发明状态值。
func TestTodoWriteSchemaHasStatuses(t *testing.T) {
	schema := string(NewTodoTool(nil).InputSchema())
	for _, s := range []string{"pending", "in_progress", "completed", "cancelled"} {
		if !strings.Contains(schema, s) {
			t.Errorf("schema 缺状态枚举 %q", s)
		}
	}
}

// 描述必须规定「何时调用/何时不调用」（官方工具描述最佳实践），且点明整表替换语义。
func TestTodoWriteDescriptionPrescribesWhenToCall(t *testing.T) {
	d := NewTodoTool(nil).Description()
	for _, want := range []string{"何时调用", "何时不调用", "完整", "in_progress"} {
		if !strings.Contains(d, want) {
			t.Errorf("todo_write 描述应含 %q：%s", want, d)
		}
	}
}
