package store

import (
	"testing"
	"time"
)

// Todo 清单 CRUD：整体替换语义（每次提交完整列表，缺项即删除）+ 级联删除。
func TestTodosReplaceAndCascade(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.CreateSession("t1", "ws", "清单会话", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession("t2", "ws", "对照会话", now); err != nil {
		t.Fatal(err)
	}

	// 初始写入 2 条
	rows := []TodoRow{{Content: "先看文档", Status: "pending"}, {Content: "再动手", Status: "pending"}}
	if err := s.ReplaceTodos("t1", rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListTodos("t1")
	if err != nil || len(got) != 2 {
		t.Fatalf("不存在的会话 ListTodos 应为空，实际 %d（err=%v）", len(got), err)
	}
	if got[0].Content != "先看文档" || got[1].Content != "再动手" {
		t.Errorf("排序/内容不对: %+v", got)
	}
	// sort 由 ReplaceTodos 按下标维护（第二个参数传入的 sort 被忽略，重新编号）
	if got[0].Sort != 0 || got[1].Sort != 1 {
		t.Errorf("sort 应按序重编号: %+v", got)
	}

	// 整体替换：只留一条且改状态 → 旧项「再动手」应消失
	if err := s.ReplaceTodos("t1", []TodoRow{{Content: "完成收尾", Status: "completed"}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ListTodos("t1")
	if len(got) != 1 || got[0].Content != "完成收尾" || got[0].Status != "completed" {
		t.Fatalf("整体替换语义失败: %+v", got)
	}

	// 会话隔离：t2 保持为空
	if got, _ := s.ListTodos("t2"); len(got) != 0 {
		t.Errorf("不同会话的清单应互相隔离: %+v", got)
	}

	// 删除会话 → 清单级联删除
	if err := s.DeleteSession("t1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListTodos("t1"); len(got) != 0 {
		t.Errorf("会话删除后清单应级联清空: %+v", got)
	}
}
