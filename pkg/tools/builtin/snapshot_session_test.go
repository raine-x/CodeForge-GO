package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codeforge/pkg/tools"
)

// Snapshot.SessionID 的作用：让撤销能**按会话**进行。
//
// 现状：undo 栈是进程级的一条直线，谁最后写谁最后撤。
//
//	session A 写了 a.txt
//	session B 写了 b.txt
//	用户在界面上点「撤销」→ 撤掉的是 B 的 b.txt
//
// 这看着还行，但一旦有第二个会话在跑，界面上的「撤销」按钮指向哪个会话
// 就完全取决于 goroutine 调度 —— 用户点「撤销我刚才的修改」可能撤掉的是
// 另一个会话的写入。
//
// 本项只做**结构层**：Snapshot 记下写入者，FS 提供会话级撤销原语。
// 界面/接口的接线需要前端带会话 id（目前没有 session header），而
// 「工作区归会话」整体属于 2.2，已被明确推迟，故不在此处改动。

// writeFileInSession 通过 write_file 工具写入，顺带走完快照与围栏逻辑。
//
// 注意：FS 本身不提供 IO 方法（只管状态：撤销栈、阅读登记、围栏），
// 实际读写一律经工具层 —— 那样才能保证快照真的被压栈了。
func writeFileInSession(t *testing.T, fs *FS, sessionID, path, content string) {
	t.Helper()
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: sessionID})
	tool := NewWriteFileTool(fs)
	res, err := tool.Execute(ctx, mustArgs(t, map[string]any{
		"path":    path,
		"content": content,
	}))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("写入不该失败: %+v", res)
	}
}

// TestSnapshotRecordsSession 快照必须记得是谁写的。
func TestSnapshotRecordsSession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	writeFileInSession(t, fs, "s1", a, "A1")
	writeFileInSession(t, fs, "s2", b, "B1")

	snaps := fs.Snapshots()
	if len(snaps) != 2 {
		t.Fatalf("应有 2 个快照，实际 %d", len(snaps))
	}
	if snaps[0].SessionID != "s1" {
		t.Errorf("第 1 个快照的会话应是 s1，实际 %q", snaps[0].SessionID)
	}
	if snaps[1].SessionID != "s2" {
		t.Errorf("第 2 个快照的会话应是 s2，实际 %q", snaps[1].SessionID)
	}
}

// TestUndoSessionPicksOwnWrite 会话级撤销只动自己的写入。
func TestUndoSessionPicksOwnWrite(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	writeFileInSession(t, fs, "s1", a, "A1")
	writeFileInSession(t, fs, "s2", b, "B1")

	// s1 撤销：应该撤掉 a.txt，而 b.txt 原封不动
	path, ok := fs.UndoSession("s1")
	if !ok {
		t.Fatalf("s1 有写入，应可撤销")
	}
	if filepath.Base(path) != "a.txt" {
		t.Errorf("s1 撤销的应是 a.txt，实际 %q", filepath.Base(path))
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("a.txt 是新建的，撤销后应被删除，实际 stat err=%v", err)
	}
	if got, _ := os.ReadFile(b); string(got) != "B1" {
		t.Errorf("b.txt 不该被动到，实际 %q", got)
	}
}

// TestUndoSessionKeepsStackOrder 连续撤销按会话内的先后顺序回退。
func TestUndoSessionKeepsStackOrder(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	writeFileInSession(t, fs, "s1", a, "A1")
	writeFileInSession(t, fs, "s2", b, "B1")
	writeFileInSession(t, fs, "s1", a, "A2") // s1 的第二次写

	path, ok := fs.UndoSession("s1")
	if !ok {
		t.Fatalf("s1 应可撤销")
	}
	if filepath.Base(path) != "a.txt" {
		t.Errorf("应撤 a.txt，实际 %q", filepath.Base(path))
	}
	// 回到 A1
	if got, _ := os.ReadFile(a); string(got) != "A1" {
		t.Errorf("a.txt 应回到 A1，实际 %q", got)
	}

	// 再撤一次仍是 s1 的第一次写
	if _, ok := fs.UndoSession("s1"); !ok {
		t.Fatalf("s1 还有一次可撤")
	}
	if _, err := os.Stat(a); !os.IsNotExist(err) {
		t.Errorf("撤到底后 a.txt 应被删除，实际 err=%v", err)
	}
}

// TestUndoSessionEmptySession 没有写入的会话撤不了，也不该误伤别人。
func TestUndoSessionEmptySession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	writeFileInSession(t, fs, "s1", a, "A1")

	if _, ok := fs.UndoSession("s9"); ok {
		t.Errorf("从未写入的会话不该能撤销")
	}
	if got, _ := os.ReadFile(a); string(got) != "A1" {
		t.Errorf("a.txt 不该被动到，实际 %q", got)
	}
}

// TestUndoStillGlobal 现有的进程级 Undo 行为不变。
//
// HTTP 的 handleUndo 目前没有会话可用（前端不带 session header），
// 它继续调 Undo()，所以这个行为不能变。
func TestUndoStillGlobal(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")

	writeFileInSession(t, fs, "s1", a, "A1")
	writeFileInSession(t, fs, "s2", b, "B1")

	path, ok := fs.Undo()
	if !ok {
		t.Fatalf("应可撤销")
	}
	if filepath.Base(path) != "b.txt" {
		t.Errorf("进程级撤销应撤最后写入的 b.txt，实际 %q", filepath.Base(path))
	}
}

// TestUndoSessionRefreshesFingerprint 会话级撤销同样要刷新指纹。
//
// 与 TestUndoRefreshesFingerprint 同一件事的会话级版本：撤销改回内容后，
// 见���过该路径的会话指纹必须对齐，否则该会话再写会被误判成外部修改。
func TestUndoSessionRefreshesFingerprint(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("v1"), 0o644); err != nil {
		t.Fatalf("预置文件失败: %v", err)
	}
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	if _, err := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{"path": a})); err != nil {
		t.Fatalf("读失败: %v", err)
	}
	writeFileInSession(t, fs, "s1", a, "v2-longer")

	if _, ok := fs.UndoSession("s1"); !ok {
		t.Fatalf("应可撤销")
	}
	// 撤销后内容回到 v1，指纹必须已刷新，否则这次写会被拒
	if _, err := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": a, "content": "v3",
	})); err != nil {
		t.Fatalf("撤销后再写失败: %v", err)
	}
	if got, _ := os.ReadFile(a); string(got) != "v3" {
		t.Errorf("内容应为 v3，实际 %q", got)
	}
}
