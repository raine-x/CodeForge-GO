package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateSkillWithoutWorkspaceMustFail 钉住「未选工作区」时不得写文件。
//
// 缺陷：create_skill 直接 `filepath.Join(t.fs.Root(), ".codeforge", ...)`。
// Root() 为空时 filepath.Join("", ".codeforge", "x", "SKILL.md") 就是
// **相对路径**，于是写到**服务进程的工作目录** —— 界面上表现为
// 「没选工作区也能创建技能」，而技能落在用户看不见的地方。
//
// 它的 Execute 也从不调 noWorkspace()，所以那道守卫根本没机会生效。
func TestCreateSkillWithoutWorkspaceMustFail(t *testing.T) {
	fs := NewFS("") // 未选择工作区
	tool := NewSkillCreatorTool(fs)

	// 先记下进程 CWD 下是否本来就有那个目录，避免误判为「工具创建的」。
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(cwd, ".codeforge", "skills", "nosuchskill")
	_, statErr := os.Stat(skillDir)
	existedBefore := statErr == nil

	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{
		"name":        "nosuchskill",
		"description": "测试",
		"content":     "内容",
	}))
	if err != nil {
		t.Fatalf("不应返回 error：%v", err)
	}
	if res != nil && res.Success {
		t.Fatalf("未选工作区时 create_skill 不该成功：%+v", res)
	}
	if res != nil && res.Error != "" {
		want := "未选择工作区"
		if !contains(res.Error, want) {
			t.Errorf("错误信息应含 %q，实际 %q", want, res.Error)
		}
	}

	if !existedBefore {
		if _, err := os.Stat(skillDir); err == nil {
			t.Errorf("未选工作区时不该在进程 CWD（%s）下创建 %s", cwd, skillDir)
		}
	}
}

// TestCreateSkillStaysInsideWorkspace 钉住围栏：技能目录必须在工作区内。
//
// skillNameRe 已经把技能名限制成 slug（防路径穿越），但 `.codeforge`
// 这一段是硬编码的，中间没有过 checkScope。若工作区根自身是软链
// 指向区外（macOS 的 /var → /private/var，或用户自己 ln -s），
// 写入就会落在围栏之外 —— 而系统不认为那是越界。
func TestCreateSkillStaysInsideWorkspace(t *testing.T) {
	fs := NewFS(t.TempDir())
	tool := NewSkillCreatorTool(fs)

	res, err := tool.Execute(context.Background(), mustArgs(t, map[string]any{
		"name":        "demo",
		"description": "测试",
		"content":     "内容",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("有工作区时应成功：%+v", res)
	}

	root := fs.Root()
	got := filepath.Join(root, ".codeforge", "skills", "demo", "SKILL.md")
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("技能文件应落在工作区内 %s：%v", got, err)
	}
}

// TestCreateSkillIsUndoable 钉住撤销栈覆盖。
//
// 缺陷：create_skill 用裸 os.WriteFile 写盘，**不压撤销栈**。
// 于是用户点「撤销」撤不掉技能 —— 而技能会立刻影响后续所有对话
// （可通过 @技能名 唤起），是一个有实际后果的写入。
//
// 期望：create_skill 走 casWrite，因此可撤销。
func TestCreateSkillIsUndoable(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	tool := NewSkillCreatorTool(fs)

	// 先建一个可读的基线文件，让 CAS 的存在性判断有依据
	base := filepath.Join(dir, "base.txt")
	if err := os.WriteFile(base, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if _, err := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": base,
	})); err != nil {
		t.Fatal(err)
	}

	skillPath := filepath.Join(dir, ".codeforge", "skills", "undoable", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("旧内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 登记指纹，否则 CAS 会因「本会话没读过」而拒绝（这正是我们要验证的差异）
	if _, err := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": skillPath,
	})); err != nil {
		t.Fatal(err)
	}

	before := fs.UndoDepth()
	res, err := tool.Execute(ctx, mustArgs(t, map[string]any{
		"name":        "undoable",
		"description": "测试",
		"content":     "新内容",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("应成功：%+v", res)
	}

	if fs.UndoDepth() != before+1 {
		t.Errorf("create_skill 应压一条撤销栈（%d → 期望 %d），实际 %d。"+
			"不压栈意味着用户点「撤销」撤不掉技能，而技能会立刻影响后续对话。",
			before, before+1, fs.UndoDepth())
		return
	}
	if _, ok := fs.Undo(); !ok {
		t.Errorf("撤销应成功")
		return
	}
	got, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("撤销后文件应还在（它原本就存在）：%v", err)
	}
	if string(got) != "旧内容" {
		t.Errorf("撤销后应还原为「旧内容」，实际 %q", got)
	}
}

// TestCreateSkillUsesAtomicWrite 钉住落盘必须原子。
//
// 裸 os.WriteFile 是「打开→截断→写」，进程写一半被杀会留半截 SKILL.md。
// 半截的 SKILL.md 会被当成合法技能加载进后续对话。
//
// 间接断言：casWrite 内部的落盘必须走 atomicWriteFile。
func TestCreateSkillUsesAtomicWrite(t *testing.T) {
	// 2.7 后原子落盘是**后端的义务**：守卫侧统一走 atomicWriteFileAt，
	// 它委托给 Backend.WriteFile。断言指向这条链，而不是具体实现
	//（本地后端是「临时文件 + rename」，远程后端各有等价物）。
	if !strings.Contains(funcCode(t, "atomicWriteFileAt"), "backend().WriteFile(") {
		t.Errorf("atomicWriteFileAt 必须委托给 Backend.WriteFile")
	}
	// 而 casWrite 必须用上它。
	if !strings.Contains(funcCode(t, "casWrite"), "atomicWriteFileAt(") {
		t.Errorf("casWrite 应经 atomicWriteFileAt 落盘")
	}
}

// TestCreateSkillRejectsDirectFileWrite 是结构断言：写入不许自己落盘。
//
// 三个具体漏洞分别是 os.MkdirAll / os.Stat / os.WriteFile。
// 逐个列出是为了让「哪一个漏了」在报错里直接可见。
//
// 例外：os.Stat 用来探测文件是否已存在（CAS 的 existed 参数需要），
// 它是只读探测、不写盘，所以**允许**。真正禁止的是 os.WriteFile ——
// 它绕开了 atomicWriteFile 的原子性。
func TestCreateSkillRejectsDirectFileWrite(t *testing.T) {
	code := funcCode(t, "skillWrite")
	if strings.Contains(code, "os.WriteFile") {
		t.Errorf("skillWrite 用了裸 os.WriteFile，绕开了 atomicWriteFile；"+
			"写一半被杀会留半截 SKILL.md，而半截技能文件会被当成合法技能加载。\n实际代码行：\n%s",
			code)
	}
	if strings.Contains(code, "os.MkdirAll") {
		t.Errorf("skillWrite 自己建目录，绕开了 fs 的路径解析。\n实际代码行：\n%s", code)
	}
	if !strings.Contains(code, "t.fs.casWrite") {
		t.Errorf("skillWrite 应经 casWrite 写入（因此可撤销、且有指纹保护）。\n实际代码行：\n%s", code)
	}
	if !strings.Contains(code, "t.fs.snapshot") {
		t.Errorf("skillWrite 应压撤销栈（snapshot），否则用户撤不掉技能。\n实际代码行：\n%s", code)
	}
	if !strings.Contains(code, "t.fs.markReadSeen") {
		t.Errorf("skillWrite 应刷指纹（markReadSeen），否则「创建→更新→更新」"+
			"第二次会被判成「被外部修改」而永久锁死。\n实际代码行：\n%s", code)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
