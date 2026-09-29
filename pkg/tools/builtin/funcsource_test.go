package builtin

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// funcSource 返回本包内某个函数的源码文本（含注释）。
func funcSource(t *testing.T, name string) string {
	t.Helper()
	fn, fset, filename := findFunc(t, name)
	start := fset.Position(fn.Body.Lbrace)
	end := fset.Position(fn.Body.Rbrace)
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", filename, err)
	}
	lines := strings.Split(string(b), "\n")
	if start.Line < 1 || end.Line > len(lines) {
		t.Fatalf("行号越界：%d..%d，共 %d 行", start.Line, end.Line, len(lines))
	}
	return strings.Join(lines[start.Line-1:end.Line], "\n")
}

// funcCode 返回本包内某个函数的**代码行**（不含注释）。
//
// 为什么必须去掉注释：`restore` 的注释里就写着「不能用裸 os.WriteFile」，
// 用 funcSource 做子串匹配会命中这段说明文字，测试红得莫名其妙。
// 用 AST 只取语句，注释天然被排除 —— 断言因此稳定，不会因为改了注释而误报。
func funcCode(t *testing.T, name string) string {
	t.Helper()
	fn, _, _ := findFunc(t, name)

	var lines []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		// 逐语句收集：把每个表达式/赋值/调用渲染成一行。
		switch n.(type) {
		case *ast.ExprStmt, *ast.AssignStmt, *ast.ReturnStmt,
			*ast.DeclStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
			*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt,
			*ast.GoStmt, *ast.DeferStmt, *ast.IncDecStmt,
			*ast.BranchStmt, *ast.SendStmt:
			var b strings.Builder
			renderNode(&b, n)
			lines = append(lines, b.String())
		}
		return true
	})
	return strings.Join(lines, "\n")
}

// renderNode 把一个 AST 节点渲染成单行文本（尽力而为，够断言用即可）。
func renderNode(b *strings.Builder, n ast.Node) {
	switch v := n.(type) {
	case *ast.ExprStmt:
		renderNode(b, v.X)
	case *ast.AssignStmt:
		for i, lhs := range v.Lhs {
			if i > 0 {
				b.WriteString(" ")
			}
			renderNode(b, lhs)
		}
		b.WriteString(" = ")
		for i, rhs := range v.Rhs {
			if i > 0 {
				b.WriteString(", ")
			}
			renderNode(b, rhs)
		}
	case *ast.ReturnStmt:
		b.WriteString("return")
		for i, r := range v.Results {
			if i == 0 {
				b.WriteString(" ")
			} else {
				b.WriteString(", ")
			}
			renderNode(b, r)
		}
	case *ast.CallExpr:
		renderNode(b, v.Fun)
		b.WriteString("(")
		for i, a := range v.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			renderNode(b, a)
		}
		b.WriteString(")")
	case *ast.SelectorExpr:
		renderNode(b, v.X)
		b.WriteString(".")
		b.WriteString(v.Sel.Name)
	case *ast.Ident:
		b.WriteString(v.Name)
	case *ast.BasicLit:
		b.WriteString(v.Value)
	case *ast.IfStmt:
		b.WriteString("if ")
		renderNode(b, v.Cond)
		b.WriteString(" { ... }")
	case *ast.ForStmt:
		b.WriteString("for { ... }")
	case *ast.RangeStmt:
		b.WriteString("for range { ... }")
	case *ast.DeferStmt:
		b.WriteString("defer ")
		renderNode(b, v.Call)
	case *ast.GoStmt:
		b.WriteString("go ")
		renderNode(b, v.Call)
	case *ast.BlockStmt:
		b.WriteString("{ ... }")
	case *ast.IncDecStmt:
		renderNode(b, v.X)
		b.WriteString("++")
	case *ast.SwitchStmt:
		b.WriteString("switch { ... }")
	case *ast.DeclStmt:
		if gd, ok := v.Decl.(*ast.GenDecl); ok {
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for i, nm := range vs.Names {
						if i > 0 {
							b.WriteString(", ")
						}
						b.WriteString(nm.Name)
					}
				}
			}
		}
	default:
		b.WriteString(fmt.Sprintf("%T", n))
	}
}

// findFunc 在本包的非测试文件里找函数，返回声明、FileSet 与所在文件名。
func findFunc(t *testing.T, name string) (*ast.FuncDecl, *token.FileSet, string) {
	t.Helper()

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

	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && fn.Name.Name == name && fn.Body != nil {
					return fn, fset, fset.Position(fn.Pos()).Filename
				}
			}
		}
	}
	t.Fatalf("在本包里找不到函数 %s —— 重命名后请同步相关断言", name)
	return nil, nil, ""
}
