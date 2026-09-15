// skill_creator_tool.go 提供 create_skill 工具（内置插件 Skill Creator 的能力）：
// 让 AI 在对话中动态创建/更新工作区技能（.codeforge/skills/<name>/SKILL.md）。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"codeforge/pkg/tools"
)

// skillNameRe 限定技能名为安全 slug（防路径穿越）。
var skillNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

// SkillCreatorTool 是技能创建工具。
type SkillCreatorTool struct{ fs *FS }

// NewSkillCreatorTool 构造 create_skill 工具。
func NewSkillCreatorTool(fs *FS) *SkillCreatorTool { return &SkillCreatorTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *SkillCreatorTool) Name() string { return "create_skill" }

// Description 实现 tools.Tool。
func (t *SkillCreatorTool) Description() string {
	return "创建或更新工作区技能（.codeforge/skills/<name>/SKILL.md）。仅在用户明确要求创建/沉淀/改进技能时使用；" +
		"写入后技能即刻生效，可通过 @技能名 或触发词唤起。同名技能会被覆盖（即迭代）。"
}

// InputSchema 实现 tools.Tool。
func (t *SkillCreatorTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("name", "技能名（英文 slug：小写字母/数字/-/_，最长 40 字符）", true).
		Str("display_name", "显示名（@ 提及 面板/菜单展示用；留空则用技能名）", false).
		Str("description", "一句话描述技能用途", true).
		Str("triggers", "触发词，逗号分隔（可选，如：部署, deploy）", false).
		Str("content", "技能正文：可独立照做的操作步骤与约定（Markdown）", true).
		Build()
}

// Execute 实现 tools.Tool。
func (t *SkillCreatorTool) Execute(_ context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	var p struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Triggers    string `json:"triggers"`
		Content     string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	p.Name = strings.TrimSpace(p.Name)
	if !skillNameRe.MatchString(p.Name) {
		return tools.Err("技能名 %q 不合法：需为小写字母/数字/-/_ 组成（1-40 字符，字母开头）", p.Name), nil
	}
	if strings.TrimSpace(p.Content) == "" {
		return tools.Err("content（技能正文）不能为空"), nil
	}

	// frontmatter + 正文
	var sb strings.Builder
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "name: %s\n", p.Name)
	if d := strings.TrimSpace(p.DisplayName); d != "" {
		fmt.Fprintf(&sb, "display_name: %s\n", strings.ReplaceAll(d, "\n", " "))
	}
	if d := strings.TrimSpace(p.Description); d != "" {
		fmt.Fprintf(&sb, "description: %s\n", strings.ReplaceAll(d, "\n", " "))
	}
	if tr := strings.TrimSpace(p.Triggers); tr != "" {
		fmt.Fprintf(&sb, "triggers: %s\n", tr)
	}
	sb.WriteString("enabled: true\n---\n\n")
	sb.WriteString(strings.TrimSpace(p.Content))
	sb.WriteString("\n")

	dir := filepath.Join(t.fs.Root(), ".codeforge", "skills", p.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.Err("创建技能目录失败: %v", err), nil
	}
	file := filepath.Join(dir, "SKILL.md")
	action := "已创建技能"
	if _, err := os.Stat(file); err == nil {
		action = "已更新技能" // 同名覆盖 = 迭代
	}
	if err := os.WriteFile(file, []byte(sb.String()), 0o644); err != nil {
		return tools.Err("写入 SKILL.md 失败: %v", err), nil
	}
	return tools.Ok(fmt.Sprintf("%s「%s」，用户可通过 @%s 或触发词唤起。", action, p.Name, p.Name)), nil
}

// RegisterSkillCreator 注册 Skill Creator 插件的工具（仅在配置启用时调用）。
func RegisterSkillCreator(reg *tools.Registry, fs *FS) {
	reg.Register(NewSkillCreatorTool(fs))
}
