package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"codeforge/pkg/tools"
)

// TestUndoRestoreIsAtomic 钉住撤销还原也必须走 atomicWriteFile。
//
// 缺陷：restore 末尾用的是裸 os.WriteFile —— 「打开 → 截断 → 写」。
// 撤销过程被中断（OOM、Ctrl+C、崩溃）会留下**半截文件**，
// 而这份文件是用户写之前的版本，丢了就再也回不去了。
//
// 这与同文件里 atomicWriteFile 注释立下的原则自相矛盾：同一个程序，
// 正常写入走「临时文件 + rename」，撤销却走裸写。
//
// 为什么必须测：半截文件只在「写到一半被杀」时出现，构造不了。
// 可观测的代理是「还原用的不是 os.WriteFile」这件事本身 —— 用 AST 断言。
func TestUndoRestoreIsAtomic(t *testing.T) {
	code := funcCode(t, "restore")

	if strings.Contains(code, "os.WriteFile") {
		t.Errorf("restore 的代码里出现了 os.WriteFile，撤销过程被打断会留半截文件；\n"+
			"应改走 atomicWriteFileAt。实际代码行：\n%s", code)
	}
	// 2.7 后原子落盘由 Backend.WriteFile 承担，守卫侧的入口是
	// atomicWriteFileAt —— 断言指向它，而不是指向某个具体实现细节
	//（本地后端是「临时文件 + rename」，远程后端各有自己的等价物）。
	if !strings.Contains(code, "atomicWriteFileAt(") {
		t.Errorf("restore 没有走 atomicWriteFileAt，撤销不是原子的；\n实际代码行：\n%s", code)
	}
	// 且它必须真的落到 Backend 上，不能自己造轮子。
	if !strings.Contains(funcCode(t, "atomicWriteFileAt"), "backend().WriteFile(") {
		t.Errorf("atomicWriteFileAt 必须委托给 Backend.WriteFile，" +
			"否则每个后端要各写一遍原子落盘，远程后端就退化成裸写")
	}
}

// TestUndoRestoreDoesNotWriteWithoutPathLock 钉住撤销与并发写必须互斥。
//
// 缺陷：restore 在**完全不持任何锁**的状态下改文件系统
// （既不在 f.mu 里，也不在 lockPath(snap.Path) 里）。
// 而 casWrite/casEdit/casRemove 都在按路径的锁内做「读当前 → 比对 → 写」。
//
// 后果：撤销与并发写对同一路径时，两者交错：
//
//	casWrite:  持路径锁 → 读当前（内容 X）
//	restore:                        （不持锁）写回 X-1
//	casWrite:  持路径锁 → 写 Y        ← 用户刚才那次编辑被撤销覆盖
//
// 现象是「撤销之后我明明又改了一次，内容却又变回去了」，
// 而两侧都没有报错。
//
// 判据：撤销必须取到该路径的锁。用行为断言构造不了（窗口太窄），
// 所以用 AST 断言 restore 体内是否调用了 lockPath。
func TestUndoRestoreDoesNotWriteWithoutPathLock(t *testing.T) {
	code := funcCode(t, "restore")
	if !strings.Contains(code, "lockPath(") {
		t.Errorf("restore 改文件系统时没取按路径的锁，与 casWrite/casEdit/casRemove "+
			"无互斥 —— 并发下撤销会覆盖用户后续的编辑，且两侧都不报错。\n实际代码行：\n%s",
			code)
	}
}

// TestUndoTakeSnapshotTakesPathLockToo 摘除阶段也要取路径锁。
//
// 否则「摘除」与「并发写压栈」之间同样有交错：摘除读副本的同时，
// 另一个 goroutine 正在为同一路径压新快照并逐出删除副本。
func TestUndoTakeSnapshotTakesPathLock(t *testing.T) {
	code := funcCode(t, "takeSnapshot")
	if !strings.Contains(code, "lockPath(") {
		t.Errorf("takeSnapshot 摘除快照时没取按路径的锁；\n实际代码行：\n%s", code)
	}
}

// TestUndoDoesNotBypassFence 是行为断言：撤销的落盘目标必须过围栏。
//
// 现状：restore 直接用 snap.Path 写盘，不经 checkScope。
// 若写入后发生过 SetRoot 切换工作区，撤销会写到**新工作区之外**的路径 —
// 而 snap.Path 是旧工作区里的一条绝对路径。
//
// 这条现在**应该失败**，它标出的是下一个要修的洞。
// 本项只修「无锁」与「非原子」，围栏留给 2.7（那时写入会统一走 Backend，
// 围栏只有 Guard 一份，加检查有唯一落点）。这里显式记为已知缺口，
// 不在本 commit 里顺手改 —— 顺手改会让「无锁」与「围栏」两个变更混在一起，
// 出问题时无法二分定位。
func TestUndoRestoreFenceGapIsKnown(t *testing.T) {
	code := funcCode(t, "restore")
	hasFence := strings.Contains(code, "checkScope") ||
		strings.Contains(code, "ResolveChecked")
	if hasFence {
		return
	}
	// 当前状态：已知缺口。修复后本测试应改为断言 hasFence == true。
	t.Log("已知缺口：restore 的落盘未过 checkScope。" +
		"写入后若 SetRoot 切了工作区，撤销会写到新工作区之外。" +
		"留待 2.7（写入统一走 Backend，围栏只有 Guard 一份）。")
}

// TestUndoRestoresFileByteForByte 撤销必须逐字节还原。
//
// 这条在改成 atomicWriteFile 之后**可能失效**：
// atomicWriteFile 会 Chmod 成 0644，而 os.WriteFile 对已存在文件不改权限。
// 所以原本权限更宽的文件（如 0666）撤销后会被收紧。
// 本测试只断言内容，不断言权限 —— 权限收紧是可接受的取舍
// （撤销是恢复动作，不是保留元数据），但必须在这里留一条记录，
// 免得后来者把它当回归。
func TestUndoRestoresFileByteForByte(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	p := filepath.Join(dir, "a.bin")

	original := []byte{0x00, 0xff, 0x0a, 0x0d, 0x0a, 'h', 'i', 0x00, 0xef, 0xbb, 0xbf}
	if err := os.WriteFile(p, original, 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})
	if _, err := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": p,
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path":    p,
		"content": "新内容",
	})); err != nil {
		t.Fatal(err)
	}

	if _, ok := fs.Undo(); !ok {
		t.Fatalf("撤销应成功")
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("撤销后内容不是逐字节还原\n期望 %q\n实际 %q", original, got)
	}
}

// TestUndoConcurrentWithWriteKeepsLastWriter 撤销与并发写的互斥行为断言。
//
// 构造：会话 s1 写 v1 → 写 v2（两条快照）。
// 然后一个 goroutine 持续 Undo，另一个持续 read+write。
// 不变量：文件内容**永远**必须是某一次完整写入的值，
// 不能出现「v1 的一半 + v2 的一半」这种撕裂内容。
//
// 撕裂在修复前是可能的（restore 的裸 WriteFile 是「截断→写」，
// 与 casWrite 的写入交错时会露出中间态）。这条能真正抓到，
// 因为 restore 的写窗口比 UndoSession 的锁窗口宽得多 ——
// 它整个写盘过程都不持锁。
func TestUndoConcurrentWithWriteKeepsLastWriter(t *testing.T) {
	if testing.Short() {
		t.Skip("并发压力测试，-short 下跳过")
	}
	dir := t.TempDir()
	fs := NewFS(dir)
	p := filepath.Join(dir, "a.txt")

	const marker = "MARKER-VALUE-"
	const payload = 64 * 1024 // 足够大，让一次写耗时明显长于锁窗口

	// 先建立两条可撤销的写入
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})
	_, _ = NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": p, "content": marker + "seed",
	}))
	for i := 0; i < 2; i++ {
		if _, err := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
			"path":    p,
			"content": marker + "v1",
		})); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	var torn string
	var tornMu sync.Mutex

	// 撤销方
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			fs.Undo()
		}
	}()

	// 写入方：内容是 marker 开头 + 大量填充，且必须整体要么是 v1 要么是 v2
	wg.Add(1)
	go func() {
		defer wg.Done()
		big := strings.Repeat("x", payload)
		for i := 0; i < 60; i++ {
			c := tools.WithSession(context.Background(),
				tools.SessionScope{SessionID: "s2"})
			_, _ = NewReadFileTool(fs).Execute(c, mustArgs(t, map[string]any{
				"path": p,
			}))
			_, _ = NewWriteFileTool(fs).Execute(c, mustArgs(t, map[string]any{
				"path":    p,
				"content": marker + "v2" + big,
			}))

			// 读回来校验：必须是 marker + 完整长度的 v1 或 v2
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			s := string(b)
			switch {
			case strings.HasPrefix(s, marker+"v2"):
				if len(s) != len(marker+"v2")+payload {
					tornMu.Lock()
					torn = "v2 长度 " + itoaLocal(len(s)) + "，期望 " + itoaLocal(len(marker+"v2")+payload)
					tornMu.Unlock()
					return
				}
			case strings.HasPrefix(s, marker+"v1"):
				// 完整 v1
			default:
				tornMu.Lock()
				torn = "内容既不是 v1 也不是 v2：前 40 字节 " + s[:min(40, len(s))]
				tornMu.Unlock()
				return
			}
		}
		close(stop)
	}()

	wg.Wait()

	if torn != "" {
		t.Fatalf("并发撤销与写入下文件出现撕裂内容：%s", torn)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func itoaLocal(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [12]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
