package agent

import (
	"strings"
	"testing"
)

// 启用后 System Prompt 注入插件段（名称/调用时机/工作流提醒）；关闭则不注入。
func TestBuiltinPluginSection(t *testing.T) {
	a := &Agent{}

	if a.builtinPluginSection() != "" {
		t.Error("未启用任何插件时不应有注入段")
	}

	a.SetSkillCreatorEnabled(true)
	sec := a.builtinPluginSection()
	for _, want := range []string{"Skill Creator", "调用时机", "技能创建工具", "使用插件 Skill Creator"} {
		if !strings.Contains(sec, want) {
			t.Errorf("注入段缺少 %q：%q", want, sec)
		}
	}
	// P0.5 防泄露：注入段不得含工具 ID（create_skill 等内部代号）
	for _, banned := range []string{"create_skill", "delegate_subagents"} {
		if strings.Contains(sec, banned) {
			t.Errorf("注入段泄露了工具 ID %q：%q", banned, sec)
		}
	}

	// 再关掉：段消失
	a.SetSkillCreatorEnabled(false)
	if a.builtinPluginSection() != "" {
		t.Error("关闭后不应有注入段")
	}
}

// systemPrompt 整链路：启用插件后基础提示词之外多出插件段。
func TestSystemPromptWithPlugin(t *testing.T) {
	a := &Agent{workDir: t.TempDir()}
	base := a.systemPrompt()
	if strings.Contains(base, "内置插件") {
		t.Fatal("未启用时不应包含插件段")
	}
	a.SetSkillCreatorEnabled(true)
	if !strings.Contains(a.systemPrompt(), "内置插件：Skill Creator") {
		t.Error("启用后 systemPrompt 应包含 Skill Creator 插件段")
	}
}

// Plan 插件：启用后注入段出现计划模式说明与计划书结构；不含工具 ID（防泄露）；关闭后消失。
func TestBuiltinPlanPluginSection(t *testing.T) {
	a := &Agent{}
	a.SetPlanEnabled(true)
	sec := a.builtinPluginSection()
	for _, want := range []string{"Plan（计划模式）", "只读规划模式", "项目计划书"} {
		if !strings.Contains(sec, want) {
			t.Errorf("Plan 注入段缺少 %q：%q", want, sec)
		}
	}
	for _, id := range []string{"read_file", "write_file", "edit_file", "run_command", "web_fetch", "save_memory", "todo_write"} {
		if strings.Contains(sec, id) {
			t.Errorf("Plan 注入段泄露了工具 ID %q：%q", id, sec)
		}
	}
	a.SetPlanEnabled(false)
	if a.builtinPluginSection() != "" {
		t.Error("关闭后不应有 Plan 注入段")
	}
	// 默认（新 Agent）builtinOn 未设置 → 不注入
	if (&Agent{}).builtinPluginSection() != "" {
		t.Error("未启用任何插件时不应有注入段")
	}
}
