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
	for _, want := range []string{"Skill Creator", "调用时机", "create_skill", "使用插件 Skill Creator"} {
		if !strings.Contains(sec, want) {
			t.Errorf("注入段缺少 %q：%q", want, sec)
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
