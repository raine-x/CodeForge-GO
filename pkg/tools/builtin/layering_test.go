package builtin

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuiltinToolsHaveNoDirectIO 钉住 2.7 的分层纪律：
// **工具体内不许再出现直接的文件系统 / 进程调用。**
//
// 2.7 把「纯 IO」抽到 backend.Backend、把「会话守卫」留在 FS。分层的全部价值
// 在于「换后端不用改工具层」。而这个价值极其脆弱：任何人在工具里写回一个
// os.WriteFile，分层就名存实亡 —— 远程后端下那行代码会静默地写到**本机**。
//
// 所以把它写成断言。违规时报错里指出具体是哪个工具的哪个方法。
//
// 例外（刻意保留，逐条说明理由）：
//   - writeSpill / removeSpillLocked / GCUndoSpill：撤销副本 scratch 目录，
//     默认在 ~/.codeforge/undo，是**进程级、跨工作区**的资源。
//     进 Backend 等于给远程后端定义「往我本地 home 目录写文件」的契约。
//   - undoSpillHome：只取用户主目录，是配置而非 IO。
//   - SkillCreatorTool.skillWrite 里的 os.Stat：CAS 的 existed 参数需要，
//     是只读探测、不写盘（真正的写已走 casWrite）。
func TestBuiltinToolsHaveNoDirectIO(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("取包目录失败：%v", err)
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析包目录失败：%v", err)
	}

	// 允许的 os./exec. 调用点（函数名 → 允许的调用）
	allowed := map[string]map[string]bool{
		// —— 撤销副本 scratch：进程级、跨工作区，不是工作区资源 ——
		"writeSpill":        {"os.MkdirAll": true, "os.CreateTemp": true, "os.Remove": true},
		"removeSpillLocked": {"os.Remove": true},
		"GCUndoSpill":       {"os.RemoveAll": true},
		"undoSpillHome":     {"os.UserHomeDir": true},
		"takeSnapshot":      {"os.ReadFile": true}, // 读 spill 副本

		// —— 命令文本扫描：要读**宿主**环境（~ 展开），是输入不是工作区 IO ——
		"commandTouchesOutside": {"os.UserHomeDir": true},

		// —— 服务端渲染浏览器截图：渲染的是**宿主浏览器**，
		//    与「工作区里有哪些文件」无关，不属于工作区 Backend ——
		"isExecutable":      {"os.Stat": true, "exec.LookPath": true},
		"renderWithBrowser": {"exec.CommandContext": true},
	}

	banned := []string{
		"os.ReadFile", "os.WriteFile", "os.Open", "os.Create",
		"os.ReadDir", "os.Stat", "os.MkdirAll", "os.Remove", "os.Rename",
		"os.Chmod", "os.CreateTemp",
		"exec.Command", "exec.CommandContext", "exec.LookPath",
		"filepath.Walk", "filepath.Glob", "filepath.EvalSymlinks",
	}

	problems := 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			// 逐函数检查
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				recv := receiverOf(fn)
				for _, call := range callsIn(fn) {
					sel, ok := call.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					qual := qualifierOf(sel.X)
					expr := qual + "." + sel.Sel.Name
					if !strings.HasPrefix(qual, "os") && !strings.HasPrefix(qual, "exec") &&
						!strings.HasPrefix(qual, "filepath") {
						continue
					}
					// 命中禁用名单？
					hit := false
					for _, b := range banned {
						if expr == b {
							hit = true
							break
						}
					}
					if !hit {
						continue
					}
					// 例外名单
					if m, ok := allowed[name]; ok && m[expr] {
						continue
					}
					problems++
					where := name + "()"
					if recv != "" {
						where = "(" + recv + ") " + where
					}
					t.Errorf("%s:%d 里出现直接 IO %s —— 2.7 后 IO 必须走 backend.Backend。\n"+
						"留在工具层等于「远程后端下这行会写到本机」，分层名存实亡。"+
						"（若确属例外，加进 allowed 并写清理由）",
						fset.Position(call.Pos()).Filename, fset.Position(call.Pos()).Line, expr)
					_ = where
				}
			}
		}
	}
	if problems == 0 {
		t.Log("全部工具体内无直接 IO —— 分层保持完好")
	}
}

// TestFenceLogicLivesOnlyInGuard 钉住「越界判断只有一份」。
//
// 现状有 4 份围栏实现（builtin 的 within/checkScope、terminal 的 absOutside、
// server 的 pathWithin、attachments 的 attachmentRelative）。它们是同一段逻辑的
// 复制品，改一处漏一处。3.2 的目标是收敛到 1 份；在那之前先钉住
// 「不要**新增**第 5 份」。
func TestFenceLogicLivesOnlyInGuard(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("取包目录失败：%v", err)
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析包目录失败：%v", err)
	}

	// 已知的围栏实现（按名字登记）。新增同类型的第 5 份会让本测试红。
	known := map[string]bool{
		"within": true, "checkScope": true, "resolveReal": true,
		"absOutside": true, "outsidePath": true, "checkSubagentScope": true,
		"commandTouchesOutside": true, "pathWithin": true,
		"attachmentRelative": true, "isUnder": true,
	}
	// 疑似新围栏的命名特征：出现 Realpath/EvalSymlinks 又带「范围/内外」判断
	suspicious := map[string]bool{
		"isOutside": true, "checkInside": true, "inWorkspace": true,
		"withinRoot": true, "isInScope": true, "scopeOK": true,
		"guardPath": true, "validatePath": true,
	}

	found := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				n := fn.Name.Name
				if known[n] {
					found[n] = true
				}
				if suspicious[n] {
					t.Errorf("发现疑似第 5 份围栏实现 %s:%d —— "+
						"「是否在工作区内」必须只有一份。"+
						"先确认它不是另一份复制品；若是，改为调用 builtin 的那份。",
						fset.Position(fn.Pos()).Filename, fset.Position(fn.Pos()).Line)
				}
			}
		}
	}
	if len(found) == 0 {
		t.Errorf("已登记的围栏函数一个都没找到 —— 它们被改名或删除了？" +
			"请同步本测试的 known 名单")
	}
}

// ---- AST 小工具 ----

func receiverOf(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func callsIn(fn *ast.FuncDecl) []ast.Expr {
	var out []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			out = append(out, c.Fun)
		}
		return true
	})
	return out
}

// qualifierOf 取 x.Y 里的 "x" 文本（只处理标识符与选择器两种常见形态）。
func qualifierOf(x ast.Expr) string {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if p, ok := v.X.(*ast.Ident); ok {
			return p.Name
		}
	}
	return ""
}
