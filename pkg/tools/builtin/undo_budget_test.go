package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 撤销栈的字节预算。
//
// 改造前只限**条数**（maxUndo=100），不限字节。而 snapshot 存的是
// **写前全量文件内容** —— 最坏情况 = 100 × 单文件体积，理论无界。
// 改一个 2 GB 文件就是一次 2 GB 快照。
//
// 三层保留策略（缺一层就不成立）：
//
//	内存      ≤ 1 MiB/条          留 Content
//	落盘副本  ≤ 64 MiB/条         留 Hash + Spill 路径，撤销时读回
//	只留指纹  > 64 MiB            Dropped，本次改动不可撤销
//
// 落盘副本这一层不能省：WithCheckpointSink 只在主循环注入，子智能体的写入
// **内存 undo 栈是唯一的撤销途径**。若对大文件一律降级为哈希，
// 子智能体改 5 MB 文件就变成不可撤销 —— 那是真的功能回退。

// spillFS 造一个把副本目录指向临时目录的 FS（否则测试会往真实
// ~/.codeforge/undo/ 里写垃圾）。
func spillFS(t *testing.T) (*FS, string, string) {
	t.Helper()
	fs, dir := newFSTest(t)
	spill := t.TempDir()
	fs.SetUndoSpillDir(spill)
	return fs, dir, spill
}

func spillFiles(t *testing.T, spill string) int {
	t.Helper()
	ents, err := os.ReadDir(spill)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(ents)
}

// TestUndoStackRespectsByteBudget 总预算超了就逐出最旧的。
func TestUndoStackRespectsByteBudget(t *testing.T) {
	fs, dir, _ := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(4, 1024, 1<<20, 1<<20) // 条数 4、总量 1KiB、单条 1MiB

	for i := 0; i < 8; i++ {
		p := filepath.Join(dir, "f.txt")
		content := strings.Repeat(string(rune('a'+i)), 300)
		if i == 0 {
			writeFile(t, p, content)
			if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
				t.Fatalf("读应成功: %+v", r)
			}
		}
		if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": content})); !r.Success {
			t.Fatalf("第 %d 次写应成功", i)
		}
	}

	var total int64
	for _, s := range fs.Snapshots() {
		total += int64(len(s.Content))
	}
	if total > 1024 {
		t.Errorf("撤销栈占 %d 字节，超出预算 1024 —— 逐出生效了吗", total)
	}
	if fs.UndoDepth() >= 8 {
		t.Errorf("栈深 %d，未逐出", fs.UndoDepth())
	}
}

// TestNewestSnapshotAlwaysUndoable 不变量：最新一条永远可撤销。
//
// 逐出永远丢最旧的，而 Undo() 撤的是栈顶 —— 这条保证「撤销上一步」
// 在任何预算下都不会失效。
func TestNewestSnapshotAlwaysUndoable(t *testing.T) {
	fs, dir, _ := spillFS(t)
	ctx := sessionCtx("s1")
	// 预算极小：写 3 次就只能保住 1 条
	fs.SetUndoLimits(100, 1, 1<<20, 1<<20)

	for i := 0; i < 3; i++ {
		p := filepath.Join(dir, "f.txt")
		content := strings.Repeat("x", 50)
		if i == 0 {
			writeFile(t, p, content)
			if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
				t.Fatalf("读应成功: %+v", r)
			}
		}
		if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": content})); !r.Success {
			t.Fatalf("第 %d 次写应成功", i)
		}
	}

	// 至少还能撤掉最后一次
	if _, ok := fs.Undo(); !ok {
		t.Fatal("预算再小，最新一条也必须可撤销")
	}
}

// TestLargeSnapshotSpillsToDisk 超单条上限就落盘副本，仍可撤销。
func TestLargeSnapshotSpillsToDisk(t *testing.T) {
	fs, dir, spill := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(100, 1<<20, 1024, 1<<20) // 单条上限 1KiB、副本上限 64MiB

	var content = strings.Repeat("v1-original-", 512) // 旧内容必须大，才会触发降级
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, content)
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatal("写应成功")
	}

	if n := spillFiles(t, spill); n != 1 {
		t.Fatalf("超 1KiB 的快照应落盘成 1 个副本，实际 %d 个", n)
	}
	snaps := fs.Snapshots()
	if len(snaps) != 1 {
		t.Fatalf("应有 1 条快照，实际 %d", len(snaps))
	}
	if len(snaps[0].Content) != 0 {
		t.Errorf("落盘副本后不应再在内存里留内容，实际 %d 字节", len(snaps[0].Content))
	}
	if snaps[0].Dropped {
		t.Errorf("4KiB 未超副本上限 64MiB，不该被丢弃")
	}

	// 撤销必须逐字节还原
	if _, ok := fs.Undo(); !ok {
		t.Fatal("落盘副本的快照应可撤销")
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("撤销后文件应被还原: %v", err)
	}
	if string(got) != content {
		t.Errorf("撤销后内容不对:\n got %q\nwant %q", got, content)
	}
	// 副本用完即删
	if n := spillFiles(t, spill); n != 0 {
		t.Errorf("副本被消费后应删除，目录里还剩 %d 个", n)
	}
}

// TestSnapshotOverSpillCeilingIsDropped 超副本上限就连副本也不落。
func TestSnapshotOverSpillCeilingIsDropped(t *testing.T) {
	fs, dir, spill := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(100, 1<<20, 1024, 1024) // 单条与副本上限都压到 1KiB

	var content = strings.Repeat("v1-original-", 512)
	p := filepath.Join(dir, "big.txt")
	writeFile(t, p, content)
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatal("写应成功")
	}

	if n := spillFiles(t, spill); n != 0 {
		t.Errorf("超副本上限不该落盘，实际 %d 个", n)
	}
	snaps := fs.Snapshots()
	if len(snaps) != 1 || !snaps[0].Dropped {
		t.Fatalf("应留下 1 条被丢弃的快照，实际 %+v", snaps)
	}
	// 撤销必须失败而不是拿空内容覆盖文件 —— 那是灾难性的数据损坏
	if _, ok := fs.Undo(); ok {
		t.Error("内容已被丢弃的快照不该报告撤销成功")
	}
	if got, _ := os.ReadFile(p); string(got) != "v2" {
		t.Errorf("撤销失败时文件不该被改动（应仍是 v2），实际 %q", got)
	}
}

// TestDroppedSnapshotInvalidatesFingerprint ⭐ 最高价值的一条。
//
// 降级（Dropped）条目没有内容，撤销时算不出指纹。如果此时**留下旧指纹**，
// 下一次写入会基于一个错误的基线通过 CAS —— 那就绕过了整个指纹机制。
//
// 必须走 delete 分支：强制后续重读。
func TestDroppedSnapshotInvalidatesFingerprint(t *testing.T) {
	fs, dir, _ := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(100, 1<<20, 1024, 1024) // 单条与副本上限都压到 1KiB，内容 6KB 超了 → 只留指纹

	p := filepath.Join(dir, "big.txt")
	writeFile(t, p, strings.Repeat("v1-original-", 512)) // 旧内容要够大才会触发降级
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatal("写应成功")
	}
	if _, ok := fs.Undo(); ok {
		t.Fatal("内容被丢弃，撤销应失败")
	}

	// 此刻指纹若还留着旧基线，下一次写就会通过 —— 必须是「未读过」状态
	_, err := fs.casWrite(ctx, p, true, []byte("v3"))
	if err == nil {
		t.Fatal("内容被丢弃的快照撤销后，指纹必须作废，否则下一次写会基于错误基线通过")
	}
}

// TestSpillRemovedOnSetRoot 切工作区必须清掉副本，否则这里立刻变成泄漏点。
func TestSpillRemovedOnSetRoot(t *testing.T) {
	fs, dir, spill := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(100, 1<<20, 1024, 1<<20)

	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, strings.Repeat("v1-original-", 512)) // 旧内容要够大才会触发降级
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatal("写应成功")
	}
	if spillFiles(t, spill) == 0 {
		t.Fatal("前置条件：应有 1 个副本")
	}

	fs.SetRoot(t.TempDir())
	if n := spillFiles(t, spill); n != 0 {
		t.Errorf("SetRoot 后副本应被清掉，实际还剩 %d 个", n)
	}
}

// TestGCUndoSpillRemovesOrphans 启动时清孤儿。
//
// 进程启动时撤销栈必然是空的 ⇒ 目录里任何文件都是上一次进程（含崩溃）
// 留下的孤儿，可以整目录删。
func TestGCUndoSpillRemovesOrphans(t *testing.T) {
	fs, _, spill := spillFS(t)
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(spill, "undo-orphan"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.GCUndoSpill(); err != nil {
		t.Fatalf("GCUndoSpill 应成功: %v", err)
	}
	if n := spillFiles(t, spill); n != 0 {
		t.Errorf("GC 后应为空，实际 %d 个", n)
	}
}

// TestUndoBudgetKeepsSmallSnapshotsInMemory 回归：小文件不该被无谓落盘。
//
// 每次编辑都写副本会让撤销变得又慢又留垃圾。
func TestUndoBudgetKeepsSmallSnapshotsInMemory(t *testing.T) {
	fs, dir, spill := spillFS(t)
	ctx := sessionCtx("s1")
	fs.SetUndoLimits(100, 1<<20, 1<<20, 1<<20) // 单条上限放宽到 1MiB

	p := filepath.Join(dir, "small.txt")
	writeFile(t, p, "v1")
	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatal("写应成功")
	}
	if n := spillFiles(t, spill); n != 0 {
		t.Errorf("小文件不该落盘，实际 %d 个副本", n)
	}
	if len(fs.Snapshots()[0].Content) == 0 {
		t.Error("小文件的内容应留在内存里")
	}
}
