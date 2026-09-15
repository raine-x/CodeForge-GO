package agent

import (
	"strings"
	"testing"
)

// 基础提示词必须带真实的运行环境与项目工具名（提示词里的工具名写错 = 模型空转）。
func TestDefaultSystemPromptEnvironmentAndTools(t *testing.T) {
	p := DefaultSystemPrompt(`D:\work\proj`)
	for _, want := range []string{
		"运行环境", `D:\work\proj`,
		"read_file", "list_dir", "search_files",
		"edit_file", "write_file", "delete_file", "run_command", "save_memory",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("基础提示词缺少 %q", want)
		}
	}
}

// 关键行为纪律必须在场：沟通克制、先读后改、注释只写为什么、改完验证、git 需明确授权。
func TestDefaultSystemPromptKeepsCoreDiscipline(t *testing.T) {
	p := DefaultSystemPrompt("/tmp/proj")
	for _, want := range []string{
		"不要用 run_command",
		"路径:行号",
		"注释只解释「为什么」",
		"先设法复现",
		"git commit",
		"HITL",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("基础提示词缺少纪律约束 %q", want)
		}
	}
}

// 未选工作区：如实声明并引导用户先选择，同时通用纪律仍在。
func TestDefaultSystemPromptWithoutWorkDir(t *testing.T) {
	p := DefaultSystemPrompt("")
	if !strings.Contains(p, "未选择") || !strings.Contains(p, "选择工作区") {
		t.Errorf("未选工作区时缺少引导：%s", p)
	}
	if !strings.Contains(p, "安全与边界") || !strings.Contains(p, "工具纪律") {
		t.Error("未选工作区时通用规则不应消失")
	}
}

// 提示词只写本项目真实存在的机制：不许混入 Cursor/Claude Code 的产品专属概念。
func TestDefaultSystemPromptHasNoForeignMechanisms(t *testing.T) {
	p := DefaultSystemPrompt("/tmp/proj")
	for _, forbidden := range []string{
		"startLine:endLine", // Cursor 的引用语法，本项目 UI 不识别
		"TodoWrite",         // 本项目没有 todo 工具
		"SwitchMode",        // 没有模式切换工具
		"run_in_background", // run_command 不支持后台任务
		"MCP FileSystem",    // MCP 工具是否可用取决于插件配置，基础段不承诺
	} {
		if strings.Contains(p, forbidden) {
			t.Errorf("基础提示词混入了本项目不存在的机制 %q", forbidden)
		}
	}
}

// 体积上限：System Prompt 每一轮都注入，膨胀会直接吃上下文预算。
func TestDefaultSystemPromptSizeBudget(t *testing.T) {
	p := DefaultSystemPrompt("/tmp/proj")
	if n := len([]rune(p)); n > 4096 {
		t.Errorf("基础提示词过长（%d 字符，上限 4096）：请精简而不是堆规则", n)
	}
}
