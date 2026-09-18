package store

import (
	"testing"
	"time"
)

// seedSession 建一个会话（检查点有外键约束，必须先把会话建出来）。
func seedSession(t *testing.T, st *Store, id string) {
	t.Helper()
	if err := st.CreateSession(id, "/tmp/ws", "测试会话", time.Now()); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
}

func TestCheckpointInsertAndList(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	if err := st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a.txt", Existed: true, OldContent: "v1"}); err != nil {
		t.Fatalf("插入失败: %v", err)
	}
	rows, err := st.ListCheckpoints("s1")
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(rows))
	}
	if rows[0].Step != 1 || rows[0].Path != "/a.txt" || !rows[0].Existed || rows[0].OldContent != "v1" {
		t.Fatalf("内容不符: %+v", rows[0])
	}
}

// 同一步骤内重复写同一文件：只保留**第一次**的旧内容（那才是这一步之前的样子）。
func TestCheckpointDedupKeepsEarliest(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 2, Path: "/a.txt", Existed: true, OldContent: "原始"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 2, Path: "/a.txt", Existed: true, OldContent: "中间态"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 2, Path: "/a.txt", Existed: true, OldContent: "更晚"})

	rows, _ := st.ListCheckpoints("s1")
	if len(rows) != 1 {
		t.Fatalf("去重失败：期望 1 条，得到 %d", len(rows))
	}
	if rows[0].OldContent != "原始" {
		t.Fatalf("应保留最早内容，得到 %q", rows[0].OldContent)
	}
}

// 不同步骤的同一路径必须分开记录（回滚要逐层回放）。
func TestCheckpointSamePathDiffSteps(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a.txt", Existed: true, OldContent: "v1"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 3, Path: "/a.txt", Existed: true, OldContent: "v2"})

	rows, _ := st.ListCheckpoints("s1")
	if len(rows) != 2 {
		t.Fatalf("期望 2 条（同路径不同步骤），得到 %d", len(rows))
	}
	// ListCheckpoints 步骤倒序：先 3 后 1
	if rows[0].Step != 3 || rows[1].Step != 1 {
		t.Fatalf("顺序不符: %d, %d", rows[0].Step, rows[1].Step)
	}
}

func TestCheckpointStepsAggregation(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a.txt", Existed: true, OldContent: "x"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/b.txt", Existed: true, OldContent: "y"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 5, Path: "/c.txt", Existed: false})

	steps, err := st.ListCheckpointSteps("s1")
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("期望 2 个回滚点，得到 %d", len(steps))
	}
	// 步骤倒序：5 在前
	if steps[0].Step != 5 || steps[0].Files != 1 {
		t.Fatalf("步骤 5 聚合不符: %+v", steps[0])
	}
	if steps[1].Step != 1 || steps[1].Files != 2 {
		t.Fatalf("步骤 1 聚合不符: %+v", steps[1])
	}
}

// CheckpointsFrom 必须包含边界步骤本身（回退到「该步之前」= 从该步开始全退）。
func TestCheckpointsFromIncludesBoundary(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a", Existed: true, OldContent: "1"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 2, Path: "/b", Existed: true, OldContent: "2"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 3, Path: "/c", Existed: true, OldContent: "3"})

	rows, err := st.CheckpointsFrom("s1", 2)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 条（步骤 2、3），得到 %d", len(rows))
	}
	// 步骤倒序，便于调用方逐条回放
	if rows[0].Step != 3 || rows[1].Step != 2 {
		t.Fatalf("顺序不符: %d, %d", rows[0].Step, rows[1].Step)
	}
}

func TestDeleteCheckpointsFrom(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a", Existed: true, OldContent: "1"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 2, Path: "/b", Existed: true, OldContent: "2"})
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 3, Path: "/c", Existed: true, OldContent: "3"})

	n, err := st.DeleteCheckpointsFrom("s1", 2)
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("期望删除 2 条，实删 %d", n)
	}
	left, _ := st.ListCheckpoints("s1")
	if len(left) != 1 || left[0].Step != 1 {
		t.Fatalf("剩余不符: %+v", left)
	}
}

// 检查点按会话隔离：A 会话的回滚不该碰到 B 会话的文件记录。
func TestCheckpointsIsolatedBySession(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")
	seedSession(t, st, "s2")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a", Existed: true, OldContent: "1"})
	_ = st.InsertCheckpoint("s2", CheckpointRow{Step: 1, Path: "/b", Existed: true, OldContent: "2"})

	rows, _ := st.ListCheckpoints("s1")
	if len(rows) != 1 || rows[0].Path != "/a" {
		t.Fatalf("会话隔离失败: %+v", rows)
	}
}

// 删除会话时检查点级联清理（外键 ON DELETE CASCADE）。
func TestCheckpointsCascadeOnSessionDelete(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")
	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/a", Existed: true, OldContent: "1"})

	if err := st.DeleteSession("s1"); err != nil {
		t.Fatalf("删除会话失败: %v", err)
	}
	rows, _ := st.ListCheckpoints("s1")
	if len(rows) != 0 {
		t.Fatalf("级联删除失败，仍剩 %d 条", len(rows))
	}
}

// Existed=false 的快照（写入前文件不存在）必须原样读回。
func TestCheckpointExistedFalseRoundtrip(t *testing.T) {
	st := openTestStore(t)
	seedSession(t, st, "s1")

	_ = st.InsertCheckpoint("s1", CheckpointRow{Step: 1, Path: "/new.txt", Existed: false})
	rows, _ := st.ListCheckpoints("s1")
	if len(rows) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(rows))
	}
	if rows[0].Existed {
		t.Fatal("Existed 应为 false")
	}
	if rows[0].OldContent != "" {
		t.Fatalf("不存在时旧内容应为空串，得到 %q", rows[0].OldContent)
	}
}
