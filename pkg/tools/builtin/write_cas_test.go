package builtin

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"codeforge/pkg/tools"
)

// CAS（compare-and-swap）写入。
//
// 1.1 的指纹校验是「读盘比对 → 之后才写」：两件事之间**仍有窗口**。
// 实测的调用顺序是读三次盘、零把锁：
//
//	old, _ := os.ReadFile(path)   // 为了算改动行数
//	requireReadSeen(...)          // 内部又读一次，比对指纹
//	snapshot(...)                 // 内部再读一次，压撤销栈
//	os.WriteFile(path, ...)       // ← 上一步比对的结果到这一步可能已过期
//
// 窗口期内的任何改动都会被后面的 WriteFile 覆盖回去，而守卫层认为一切正常。
//
// CAS 把「读当前 → 比对 → 写」收进**同一把按路径的锁**，并用
// 「临时文件 + rename」落盘：读方永远看不到半截内容。
//
// 这一项是**语义变更**（并发写的失败方式从「后写覆盖先写」变成「后者被拒」），
// 因此单独一轮、独立回归。

// TestConcurrentWritesOnlyOneWins 核心用例：两个会话都通过了读门禁，
// 写同一个文件时只有一个能成功。
//
// 改造前：两次都成功，后写的静默覆盖先写的 —— 没有报错，也没有任何痕迹
// 表明有一份改动被丢了。
func TestConcurrentWritesOnlyOneWins(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	// 两个会话都先读到 v1 —— 两边都合法
	for _, sid := range []string{"s1", "s2"} {
		if r, _ := NewReadFileTool(fs).Execute(sessionCtx(sid), mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
			t.Fatalf("%s 读应成功: %+v", sid, r)
		}
	}

	// 并发写。start 是闸门，让两个 goroutine 尽量同时进入临界区。
	var start sync.WaitGroup
	var done sync.WaitGroup
	ok := make([]bool, 2)
	sids := []string{"s1", "s2"}
	start.Add(1)
	for i, sid := range sids {
		done.Add(1)
		go func(i int, sid string) {
			defer done.Done()
			start.Wait()
			r, _ := NewWriteFileTool(fs).Execute(sessionCtx(sid), mustArgs(t, map[string]string{
				"path": p, "content": "v2-from-" + sid,
			}))
			ok[i] = r != nil && r.Success
		}(i, sid)
	}
	start.Done()
	done.Wait()

	if ok[0] == ok[1] {
		t.Fatalf("两个并发写都成功或都失败了（%+v）—— 应恰好一个成功", ok)
	}
	// 落盘内容必须来自成功的那次，不能是失败者的内容
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读回落文件失败: %v", err)
	}
	winner := sids[0]
	if !ok[0] {
		winner = sids[1]
	}
	if got := string(raw); got != "v2-from-"+winner {
		t.Errorf("文件内容是 %q，但成功的是 %s —— 失败者的改动被写进去了", got, winner)
	}
}

// TestCasWriteRejectsUnreadFile 没有读过就写，必须被拒。
// CAS 取代了 requireReadSeen，所以这条原有约束不能丢。
func TestCasWriteRejectsUnreadFile(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	if _, err := fs.casWrite(sessionCtx("s9"), p, true, []byte("v2")); err == nil {
		t.Fatal("未读过就写入未被拒绝")
	}
	if got, _ := os.ReadFile(p); string(got) != "v1" {
		t.Errorf("被拒的写入不该改动文件，实际 %q", got)
	}
}

// TestCasWriteRejectsForeignChange 外部改过就写，必须被拒（1.1 的语义保留）。
func TestCasWriteRejectsForeignChange(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	if r, _ := NewReadFileTool(fs).Execute(sessionCtx("s1"), mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	writeFile(t, p, "v2-external")

	if _, err := fs.casWrite(sessionCtx("s1"), p, true, []byte("v3")); err == nil {
		t.Fatal("外部改过之后写入仍被放行")
	}
	if got, _ := os.ReadFile(p); string(got) != "v2-external" {
		t.Errorf("被拒的写入不该改动文件，实际 %q", got)
	}
}

// TestCasWriteRejectsFileAppeared 新建文件的 CAS：
// 判定「不存在」之后文件被别人创建了，写入必须被拒。
//
// 新建路径上原来完全没有这类防护 —— 覆盖别人的文件且不留痕迹。
func TestCasWriteRejectsFileAppeared(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "new.txt")

	// 外部抢先创建
	if err := os.WriteFile(p, []byte("someone-else"), 0o644); err != nil {
		t.Fatalf("外部创建失败: %v", err)
	}

	// 写入方以为「新建」（existed=false），但文件已存在 → 拒绝
	if _, err := fs.casWrite(sessionCtx("s1"), p, false, []byte("mine")); err == nil {
		t.Fatal("文件已被外部创建，写入仍被放行 —— 覆盖了别人的文件")
	}
	if got, _ := os.ReadFile(p); string(got) != "someone-else" {
		t.Errorf("被拒的写入不该改动文件，实际 %q", got)
	}
}

// TestCasWriteAtomicNoTempLeftover 原子落盘不留下临时文件。
func TestCasWriteAtomicNoTempLeftover(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")
	if r, _ := NewReadFileTool(fs).Execute(sessionCtx("s1"), mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if _, err := fs.casWrite(sessionCtx("s1"), p, true, []byte("v2")); err != nil {
		t.Fatalf("CAS 写入应成功: %v", err)
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() != "a.txt" {
			t.Errorf("落盘后目录里多了残留文件: %s", e.Name())
		}
	}
}

// TestCasWriteReportsContentBefore CAS 要把「写入前的内容」一并返回，
// 否则调用方还得再读一次盘 —— 而那次读已不在临界区里，值可能过期。
func TestCasWriteReportsContentBefore(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "line1\nline2\n")
	if r, _ := NewReadFileTool(fs).Execute(sessionCtx("s1"), mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	before, err := fs.casWrite(sessionCtx("s1"), p, true, []byte("line1\nline2\nline3\n"))
	if err != nil {
		t.Fatalf("CAS 写入应成功: %v", err)
	}
	if string(before) != "line1\nline2\n" {
		t.Errorf("返回的旧内容不对: %q", before)
	}
}

// TestDeleteRequiresRead 删一个没读过的文件必须被拒。
//
// 改造前 delete_file 完全没有读门禁 —— 删没读过的文件与覆盖它是同一类丢数据，
// 而且更不可逆（覆盖至少能看出文件变小了，删除后什么都没了）。
func TestDeleteRequiresRead(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	r, _ := NewDeleteFileTool(fs).Execute(sessionCtx("s9"), mustArgs(t, map[string]string{"path": p}))
	if r.Success {
		t.Fatal("未读过就删除未被拒绝")
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("被拒的删除不该动文件: %v", err)
	}

	// 读过后即可删
	if r, _ := NewReadFileTool(fs).Execute(sessionCtx("s9"), mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewDeleteFileTool(fs).Execute(sessionCtx("s9"), mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读过后应可删: %+v", r)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("删除后文件应不存在，stat err=%v", err)
	}
}

// TestDeleteIsUndoable 删除必须能被撤销，且撤销后内容逐字节还原。
// 覆盖 snapshot 记错内容的风险（那会让撤销「看起来成功」但文件没回到原样）。
func TestDeleteIsUndoable(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	const content = "line1\nline2\nline3"
	writeFile(t, p, content)
	ctx := sessionCtx("s1")
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewDeleteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatal("删除应成功")
	}
	if _, ok := fs.Undo(); !ok {
		t.Fatal("撤销应可用")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("撤销后文件应被还原: %v", err)
	}
	if string(got) != content {
		t.Errorf("撤销后内容不对:\n got %q\nwant %q", got, content)
	}
}

// TestCasErrorIsDistinguishable CAS 失败要能被审计与提示区分，
// 且文案对模型可读（不是裸的 CAS 术语）。
func TestCasErrorIsDistinguishable(t *testing.T) {
	fs, dir := newFSTest(t)
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	_, err := fs.casWrite(sessionCtx("s9"), p, true, []byte("v2"))
	if err == nil {
		t.Fatal("应当报错")
	}
	var stale *tools.ErrStaleContent
	if !errors.As(err, &stale) {
		t.Fatalf("应返回 *tools.ErrStaleContent 以便审计区分，实际 %T: %v", err, err)
	}
	if stale.Message() == "" {
		t.Error("错误信息不应为空")
	}
}
