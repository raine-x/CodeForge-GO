package agent

import (
	"strings"
	"testing"
)

// 提示词必须带真实的运行环境，且不依赖具体工具 ID（P0.5：工具由 Tool Definition
// 下发，System Prompt 不再列举内部名称 —— 具体 ID 见下方防泄露测试）。
func TestDefaultSystemPromptEnvironmentAndTools(t *testing.T) {
	p := DefaultSystemPrompt(`D:\work\proj`)
	for _, want := range []string{
		"运行环境", `D:\work\proj`,
		"中文功能名", "JSON Schema", "专用能力",
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
		"不要用 shell", "shell 的 cat",
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
		"TodoWrite",         // 工具名以中文「任务清单」对外呈现，禁止大写英文写法
		"SwitchMode",        // 没有模式切换工具
		"run_in_background", // run_command 不支持后台任务
		"MCP FileSystem",    // MCP 工具是否可用取决于插件配置，基础段不承诺
	} {
		if strings.Contains(p, forbidden) {
			t.Errorf("基础提示词混入了本项目不存在的机制 %q", forbidden)
		}
	}
}

// P0.5 防泄露：System Prompt 不出现任何工具 ID —— 工具名由 Tool Definition 提供，
// 隐藏工具不应从 Prompt 侧再泄露一遍名字。中文能力名可以有，裸 ID 一律禁止。
func TestSystemPromptLeaksNoToolIDs(t *testing.T) {
	p := DefaultSystemPrompt("/tmp/proj")
	// 全部内置工具 ID 黑名单：出现在 Prompt 里 = 泄露通道。
	for _, id := range []string{
		"read_file", "write_file", "edit_file", "delete_file", "list_dir",
		"search_files", "run_command", "save_memory", "todo_write",
		"web_fetch", "web_search", "subagents", "skill_creator",
	} {
		if strings.Contains(p, id) {
			t.Errorf("System Prompt 泄露了工具 ID %q（应只存在于 Tool Definition）", id)
		}
	}
	// 对照表形态（中文=ID）也禁止
	for _, pair := range []string{"运行命令=run_command", "读取网页=web_fetch", "编辑文件=edit_file", "保存记忆=save_memory"} {
		if strings.Contains(p, pair) {
			t.Errorf("System Prompt 不应含 中文↔ID 对照 %q", pair)
		}
	}
	// 中文功能名规则本身必须还在（引导模型对用户说中文，这是沟通纪律不是泄露）
	if !strings.Contains(p, "中文功能名") {
		t.Error("应保留「对用户用中文功能名」的沟通纪律")
	}
	// 全覆盖纪律必须还在：只覆盖「描述动作 / 被问能力」两种场景是不够的 ——
	// 实测模型会在计划、步骤说明里复述工具名（"我该调用 xxx"）。
	// 这条断言钉住「凡用户能看到的文字都不得出现工具 ID」这句，防止以后精简提示词时被删掉。
	if !strings.Contains(p, "都不得出现任何工具 ID") {
		t.Error("应保留「凡用户能看到的文字都不得出现工具 ID」的全覆盖披露纪律")
	}
	// 联网小节标题用中文功能名
	if !strings.Contains(p, "## 联网（读取网页 / 搜索网页）") {
		t.Error("联网小节标题应为中文功能名")
	}
}

// 体积上限：System Prompt 每一轮都注入，膨胀会直接吃上下文预算。
func TestDefaultSystemPromptSizeBudget(t *testing.T) {
	p := DefaultSystemPrompt("/tmp/proj")
	if n := len([]rune(p)); n > 4096 {
		t.Errorf("基础提示词过长（%d 字符，上限 4096）：请精简而不是堆规则", n)
	}
}

// 内置插件注入段（builtinPluginSection）同样不得含工具 ID：它随 System Prompt
// 进入模型上下文，create_skill / delegate_subagents 一旦混入即构成第二泄露通道。
func TestBuiltinPluginSectionLeaksNoToolIDs(t *testing.T) {
	ag := &Agent{}
	ag.builtinOn = map[string]bool{"skill_creator": true, "multi_agent": true}
	sec := ag.builtinPluginSection()
	if sec == "" {
		t.Fatal("两个插件都启用时注入段不应为空")
	}
	for _, id := range []string{"create_skill", "delegate_subagents"} {
		if strings.Contains(sec, id) {
			t.Errorf("内置插件注入段泄露了工具 ID %q", id)
		}
	}
}
