package builtin

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"codeforge/pkg/tools"
)

// TestUndoSessionNeverUndoesAnotherSession 是 UndoSession 的并发护栏。
//
// 缺陷：`UndoSession` 曾先在锁内查出绝对下标，解锁后带着这个**已可能失效**的
// 下标去摘除。而摘除只在 `idx < 0` 时重算下标，正下标不复核。于是：
//
//  1. 栈 = [A(s1), B(s2)]
//  2. G1 UndoSession("s2") 在锁内查到 idx=1，解锁
//  3. G2 pushSnapshot 追加并触发逐出（预算超限 → 头部左移，所有下标 -1）
//  4. G1 摘走的是 G2 刚写的条目
//
// 后果最严重的一种：`UndoSession("s2")` **报告成功**并返回 s3 的路径，
// s2 的改动原封不动，而 s3 有一笔改动被静默回滚 —— s3 自己完全不知情。
//
// 判据用「UndoSession 返回的路径必须属于本会话」：这个不变量无需控制调度，
// 无论竞态何时发生都会立刻违反。
//
// ⚠️ **这条是概率性护栏，不是确定性复现。** 实测在修复前的代码上跑过
// 多核 60 轮×12 写、单核 400 轮×6 写（含 GOMAXPROCS(1) 把锁交接拉宽成
// 完整调度），都未触发 —— 窗口宽度只有几十纳秒，公开 API 无法稳定命中。
//
// 所以它保护的是「不要把定位与摘除拆开」这个**后果**在长时间运行中
// 不会重新出现；确定性的保护来自下面两条顺序测试 —— 它们钉住
// 「撤销只消耗自己那条」这个可观测不变量。
//
// 修复方式是 takeSnapshot 把定位、摘除、读副本、删副本合并进一把锁。
func TestUndoSessionNeverUndoesAnotherSession(t *testing.T) {
	if testing.Short() {
		t.Skip("并发压力测试，-short 下跳过")
	}

	// GOMAXPROCS(1)：让正阻塞在 f.mu 上的 goroutine 在 Unlock 时被直接交接，
	// 把窗口从「几十纳秒」拉宽到「一个完整调度」。不是消除不确定性，
	// 而是让「测试不报错」这件事有意义。
	prev := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prev)

	const rounds = 60
	const writesPerRound = 6

	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		fs := NewFS(dir)
		// maxCount 刻意设小，让追加很快触发头部逐出（下标失效的三条路径之一）。
		fs.SetUndoLimits(3, 0, 0, 0)

		sids := []string{"s1", "s2", "s3"}

		// 先让每个会话各写一条，栈里三个会话都有。
		for _, sid := range sids {
			writeFileInSession(t, fs, sid, filepath.Join(dir, sid+".txt"), sid+"-v0")
		}

		var wg sync.WaitGroup
		start := make(chan struct{})
		var mu sync.Mutex
		var violations []string

		for _, sid := range sids {
			wg.Add(1)
			go func(sid string) {
				defer wg.Done()
				<-start
				path := filepath.Join(dir, sid+".txt")
				for i := 1; i < writesPerRound; i++ {
					ctx := tools.WithSession(context.Background(),
						tools.SessionScope{SessionID: sid})
					// 撤销会让指纹作废（内容回到上一版），所以必须重新读一次
					// 才能继续写 —— 否则「先读后写」门禁会拒掉后续所有写入，
					// 循环实际只跑一轮，竞态窗口根本建立不起来。
					NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
						"path": path,
					}))

					tool := NewWriteFileTool(fs)
					if _, err := tool.Execute(ctx, mustArgs(t, map[string]any{
						"path":    path,
						"content": sid + "-v" + strconv.Itoa(i),
					})); err != nil {
						continue
					}
					// 撤销自己最近一次
					p, ok := fs.UndoSession(sid)
					if !ok {
						continue
					}
					want := filepath.Base(path)
					if filepath.Base(p) != want {
						mu.Lock()
						violations = append(violations,
							"UndoSession("+sid+") 返回 "+filepath.Base(p)+"，期望 "+want)
						mu.Unlock()
						return
					}
				}
			}(sid)
		}

		close(start)
		wg.Wait()

		if len(violations) > 0 {
			t.Fatalf("round %d 撤错了会话：%v", round, violations)
		}
	}
}

// TestUndoSessionStackShrinksOnlyByItsOwnEntries 检查栈的消耗速率。
//
// 每一次成功的 UndoSession(sid) 都必须消耗**一条 sid 自己的**条目。
// 若实现有下标漂移，别的会话的条目会被误消耗 —— 表现为「我撤了 N 次，
// 但栈里属于自己的条目没减少 N 条」。
//
// 这个断言比上一条更间接，但对「撤销成功却什么也没撤」这一类退化也敏感。
func TestUndoSessionStackShrinksOnlyByItsOwnEntries(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	const n = 20
	for i := 0; i < n; i++ {
		writeFileInSession(t, fs, "s1", filepath.Join(dir, "s1.txt"), "v"+strconv.Itoa(i))
	}
	for i := 0; i < n; i++ {
		writeFileInSession(t, fs, "s2", filepath.Join(dir, "s2.txt"), "v"+strconv.Itoa(i))
	}
	if got := fs.UndoDepth(); got != 2*n {
		t.Fatalf("写入后栈深应为 %d，实际 %d", 2*n, got)
	}

	// 撤 s1 五次，每次都必须减少一条，且撤的都是 s1 的。
	for i := 0; i < 5; i++ {
		before := fs.UndoDepth()
		p, ok := fs.UndoSession("s1")
		if !ok {
			t.Fatalf("第 %d 次撤销 s1 失败", i)
		}
		if got := filepath.Base(p); got != "s1.txt" {
			t.Fatalf("第 %d 次撤销 s1 撤到了 %s", i, got)
		}
		if after := fs.UndoDepth(); after != before-1 {
			t.Fatalf("第 %d 次撤销后栈深 %d，应为 %d", i, after, before-1)
		}
	}

	// 剩下的 s2 条目必须完好。
	for i := 0; i < 5; i++ {
		p, ok := fs.UndoSession("s2")
		if !ok {
			t.Fatalf("第 %d 次撤销 s2 失败 —— s1 的撤销误伤了 s2 的条目", i)
		}
		if got := filepath.Base(p); got != "s2.txt" {
			t.Fatalf("第 %d 次撤销 s2 撤到了 %s", i, got)
		}
	}
}

// TestUndoSessionWithEmptyStackForOtherSession 钉住「撤销不存在的会话」是
// 失败而不是「撤栈顶」。这条在修复前就通过，作为回归保护。
func TestUndoSessionWithEmptyStackForOtherSession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	only := filepath.Join(dir, "only.txt")
	other := filepath.Join(dir, "other.txt")

	writeFileInSession(t, fs, "s1", only, "S1")
	writeFileInSession(t, fs, "s2", other, "S2")

	if _, ok := fs.UndoSession("s1"); !ok {
		t.Fatalf("撤销 s1 应成功")
	}
	if _, ok := fs.UndoSession("s3"); ok {
		t.Errorf("撤销不存在的会话 s3 不应成功 —— 它撤掉了栈顶（属于 s2）")
	}
	if _, ok := fs.UndoSession("s2"); !ok {
		t.Errorf("s2 的条目应仍可撤销")
	}
}
