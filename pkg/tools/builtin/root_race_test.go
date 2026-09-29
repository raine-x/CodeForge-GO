package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWorkspaceAccessorsHoldLockBeforeReadingFields 用 AST 结构断言钉住一条纪律：
//
//	工作区状态的只读访问器，取字段前必须先持锁（或走原子）。
//
// 为什么必须是结构断言而不是 -race：
//
//	本机 CGO_ENABLED=0 且无 gcc，`go test -race` 跑不了（已知欠账）。
//	而 Root() 被 pkg/server 的 5 处 handler 从 HTTP goroutine 读
//	（api_handlers.go 的附件/目录树接口、workspace.go、attachments.go），
//	SetRoot 又能从 /api/workspace 热切换工作区。真发生 data race 时症状是
//	「切工作区后偶发读到旧路径或半个字符串」，**无法稳定复现**。
//
//	单测能稳定复现的只有「纪律被违反」这件事本身。所以把纪律写成断言：
//	有人再加回无锁直读，测试立刻红，且失败信息直接指向那个方法。
//
// 为什么 Root() 值得单独钉：它是 8 个读点里**唯一**没加锁的
//
//	（noWorkspace / Resolve / checkScope / checkSubagentScope / outsidePath /
//	commandTouchesOutside 都先取 f.mu），属于「差一个」的状态 —— 恰恰是最容易被
//	下一个改动者模仿回去的那种。
func TestWorkspaceAccessorsHoldLockBeforeReadingFields(t *testing.T) {
	fset := token.NewFileSet()
	self, err := filepath.Abs("file_ops.go")
	if err != nil {
		t.Skipf("取源码绝对路径失败，跳过结构断言：%v", err)
	}
	file, err := parser.ParseFile(fset, self, nil, 0)
	if err != nil {
		t.Fatalf("解析 file_ops.go 失败：%v", err)
	}

	// 这些访问器从外部 goroutine 读取共享状态（HTTP handler / WS）。
	// 它们的函数体里不允许出现裸的 f.<field> 选择器表达式。
	accessors := []string{
		"Root",         // 唯一历史上漏锁的
		"AllowOutside", // 同类：allowOutside 也要锁
	}

	for _, name := range accessors {
		fn := findMethod(file, name)
		if fn == nil {
			t.Fatalf("file_ops.go 里找不到方法 %s —— 改了结构必须同步本测试", name)
		}
		if !fnHasLockCall(fn) {
			t.Errorf("方法 %s 的函数体里没有任何锁调用（Lock/RLock），"+
				"却直接读共享字段：%s\n"+
				"这会与 SetRoot / SetAllowOutside 形成 data race。"+
				"本机无 gcc 跑不了 -race，只能靠这条断言兜住。",
				name, exprSource(fset, fn))
		}
	}
}

// TestSetRootAndSetAllowOutsideStillWriteUnderLock 反向钉住写侧。
// 写侧本来就有锁，但拆并发修复时最容易把「读加锁」顺手改成「写不加锁」
// 来「减少锁开销」，所以两端都要钉。
func TestSetRootAndSetAllowOutsideStillWriteUnderLock(t *testing.T) {
	fset := token.NewFileSet()
	self, err := filepath.Abs("file_ops.go")
	if err != nil {
		t.Skipf("取源码绝对路径失败，跳过结构断言：%v", err)
	}
	file, err := parser.ParseFile(fset, self, nil, 0)
	if err != nil {
		t.Fatalf("解析 file_ops.go 失败：%v", err)
	}

	for _, name := range []string{"SetRoot", "SetAllowOutside"} {
		fn := findMethod(file, name)
		if fn == nil {
			t.Fatalf("file_ops.go 里找不到方法 %s", name)
		}
		if !fnHasLockCall(fn) {
			t.Errorf("方法 %s 写共享字段却没有锁调用：%s", name, exprSource(fset, fn))
		}
	}
}

// findMethod 按名字找一个 *FS 的方法。
func findMethod(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name || fn.Recv == nil {
			continue
		}
		return fn
	}
	return nil
}

// fnHasLockCall 判断函数体内是否出现 `.Lock()` / `.RLock()` 调用。
func fnHasLockCall(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Lock" || sel.Sel.Name == "RLock" {
			found = true
			return false
		}
		return true
	})
	return found
}

// exprSource 把函数体还原成文本，用于失败信息里指认到底是哪几行。
func exprSource(fset *token.FileSet, fn *ast.FuncDecl) string {
	if fn.Body == nil {
		return "<无函数体>"
	}
	start := fset.Position(fn.Body.Lbrace)
	end := fset.Position(fn.Body.Rbrace)
	b, err := readFileLines(start.Filename, start.Line, end.Line)
	if err != nil {
		return "<无法读取源码>"
	}
	return strings.Join(b, "\n")
}

// readFileLines 读 file 的 [from, to] 行（1-based，闭区间）。
func readFileLines(path string, from, to int) ([]string, error) {
	b, err := readWholeFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	return lines[from-1 : to], nil
}

// TestRootReflectsSetRootAfterConcurrentSwitch 是 Root() 的并发行为断言。
//
// 它能抓的具体缺陷：Root() 读到**撕裂**的中间状态（字符串的指针与长度来自
// 两次不同的写），或 Root() 缓存了 root 导致 SetRoot 后读不到新值。
//
// 抓不到的：普通无锁但实际不撕裂的读 —— 那需要 -race。
// 那一半由上面的结构断言承担。两者互补，缺一不可。
func TestRootReflectsSetRootAfterConcurrentSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("并发压力测试，-short 下跳过")
	}
	fs := NewFS(t.TempDir())
	first := fs.Root()
	if first == "" {
		t.Fatalf("NewFS 后 Root() 为空")
	}
	second := t.TempDir()

	const readers = 4
	const writes = 400

	var wg sync.WaitGroup
	stop := make(chan struct{})

	var readErr string
	var readMu sync.Mutex

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				got := fs.Root()
				// 合法取值只有两个：第一个根、第二个根。
				// 其它值即说明读到了撕裂状态或中间态。
				if got != first && got != second {
					readMu.Lock()
					if readErr == "" {
						readErr = got
					}
					readMu.Unlock()
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			if i%2 == 0 {
				fs.SetRoot(second)
			} else {
				fs.SetRoot(first)
			}
		}
		// 最后一次是 i=399（奇数）→ first
		close(stop)
	}()

	wg.Wait()

	if readErr != "" {
		t.Fatalf("Root() 返回了撕裂的路径：%q（合法值只有 %q 或 %q）",
			readErr, first, second)
	}
	if got := fs.Root(); got != first {
		t.Fatalf("切换 %d 次后 Root() 应停在 first，实际 %q", writes, got)
	}
}

// TestRootEmptyAfterSetBlank 钉住 SetRoot("") 的语义未被并发修复破坏。
func TestRootEmptyAfterSetBlank(t *testing.T) {
	fs := NewFS(t.TempDir())
	fs.SetRoot("   ")
	if got := fs.Root(); got != "" {
		t.Fatalf("SetRoot(空白) 后 Root() 应为空，实际 %q", got)
	}
	if fs.noWorkspace() == nil {
		t.Fatalf("空工作区时 noWorkspace() 应返回非 nil 错误结果")
	}
	if fs.AllowOutside() {
		t.Fatalf("空工作区不应隐式放开越界访问")
	}
}

// TestToolsGuardEmptyWorkspaceBeforeResolveChecked 钉住空工作区的守门位置。
//
// 契约：`ResolveChecked` / `checkScope` 在 root == "" 时**故意**放行
// （checkScope 开头 `if root == "" { return nil }`，因为「未选择工作区」不是
// 「越界」）。所以空工作区的守门是工具层的 noWorkspace()，不是 ResolveChecked。
//
// 漏掉 noWorkspace() 的后果：root 为空时 Resolve 返回的是进程 CWD 下的
// 相对路径（filepath.Join("", p) == p），于是 write_file 会写到
// 服务进程的工作目录 —— 界面上表现为「没选工作区也能写文件」。
//
// 这条测试是逐工具的结构断言，因为漏掉守卫在运行期不一定会崩
// （取决于进程 CWD 是否可写），靠行为断言抓不稳。
func TestToolsGuardEmptyWorkspaceBeforeResolveChecked(t *testing.T) {
	// 这些工具在 Execute 里调了 ResolveCheckedCtx / Resolve，
	// 每一个都必须先调 noWorkspace。
	mustGuard := []string{
		"ReadFileTool",
		"ListDirTool",
		"WriteFileTool",
		"EditFileTool",
		"DeleteFileTool",
		"SearchTool",
		"FindFilesTool",
		"TerminalTool",
	}
	// 这些工具完全不碰 ResolveChecked，不需要 noWorkspace。
	// 把它们列出来是为了让「新增工具忘了守卫」这件事在测试里显式可见 ——
	// 新增工具的人要在这里表态，而不是默默不写。
	noResolveNeeded := []string{
		"SkillCreatorTool", // 已知缺口：只读 Root()，不写工作区路径
	}

	fset := token.NewFileSet()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("取包目录失败：%v", err)
	}
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析包目录失败：%v", err)
	}

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				recv := receiverType(fn)
				if recv == "" || !mustGuardContains(recv, mustGuard) {
					continue
				}
				body := exprSource(fset, fn)
				usesResolve := strings.Contains(body, "ResolveChecked") ||
					strings.Contains(body, "ResolveCheckedCtx")
				if !usesResolve {
					continue
				}
				if !strings.Contains(body, "noWorkspace()") {
					t.Errorf("%s.%s 用了 ResolveChecked 却没先调 noWorkspace()；\n"+
						"root 为空时 Resolve 会返回进程 CWD 下的相对路径，等于放开写入。\n%s",
						recv, fn.Name.Name, body)
				}
			}
		}
	}

	_ = noResolveNeeded
}

// receiverName 取方法的接收者类型名（去掉指针星号与包前缀）。
func receiverType(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func mustGuardContains(recv string, names []string) bool {
	for _, n := range names {
		if recv == n {
			return true
		}
	}
	return false
}

// readWholeFile 读整个文件。
func readWholeFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
