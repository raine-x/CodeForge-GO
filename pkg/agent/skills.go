// skills.go 实现技能系统：SKILL.md 目录约定 + 触发词匹配注入。
//
// 约定：技能存放在 <workspace>/.codeforge/skills/<name>/SKILL.md。
// SKILL.md 头部 YAML frontmatter 声明元信息：
//
//	---
//	name: code-review
//	description: 代码审查技能，按项目规范审查变更
//	triggers: 审查, code review, review
//	enabled: true
//	---
//	（正文为注入给模型的指令内容）
//
// 每轮注入策略：
//   - 索引段（全部技能的名称+描述+触发词，常驻）让模型知道有哪些技能可 @提及；
//   - 用户输入命中某技能触发词（或显式 @技能名）时，把该 SKILL.md 正文注入当轮。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Skill 是一个已解析的技能。
type Skill struct {
	Name        string
	DisplayName string // 展示名（@ 面板/菜单用；缺省回退 Name，见 server/api_handlers.go）
	Description string
	Triggers    []string
	Enabled     bool
	Body        string // frontmatter 之后的正文指令
	Path        string
}

// skillsDir 返回当前工作区的技能目录。
func (a *Agent) skillsDir() string {
	return filepath.Join(a.WorkDir(), ".codeforge", "skills")
}

// loadSkills 扫描并解析工作区全部技能（enabled 缺省视为 true）。
// 目录不存在返回 nil（全新工作区，属正常态）。
func (a *Agent) loadSkills() []Skill {
	entries, err := os.ReadDir(a.skillsDir())
	if err != nil {
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(a.skillsDir(), e.Name(), "SKILL.md")
		if s, ok := parseSkillFile(p); ok {
			out = append(out, s)
		}
	}
	return out
}

// parseSkillFile 解析单个 SKILL.md。
func parseSkillFile(path string) (Skill, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, false
	}
	s := parseSkill(string(data))
	if s.Name == "" {
		s.Name = strings.TrimSuffix(filepath.Base(filepath.Dir(path)), "")
	}
	s.Path = path
	return s, true
}

// parseSkill 从文本解析 frontmatter 与正文（无 frontmatter 时整体作为正文）。
func parseSkill(text string) Skill {
	s := Skill{Enabled: true}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "---") {
		s.Body = text
		return s
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		s.Body = text
		return s
	}
	fm := text[3 : end+3]
	// frontmatter 之后去掉结尾分隔符，正文从其后开始
	rest := text[end+3+3:]
	s.Body = strings.TrimSpace(strings.TrimPrefix(rest, "---"))

	for _, line := range strings.Split(fm, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "name":
			s.Name = v
		case "display_name":
			s.DisplayName = v
		case "description":
			s.Description = v
		case "triggers":
			for _, t := range strings.Split(v, ",") {
				if t = strings.TrimSpace(t); t != "" {
					s.Triggers = append(s.Triggers, t)
				}
			}
		case "enabled":
			s.Enabled = !strings.EqualFold(v, "false")
		}
	}
	return s
}

// skillIndexSection 生成技能索引段（常驻注入：名称+描述+触发词）。
func skillIndexSection(skills []Skill) string {
	enabled := filterEnabled(skills)
	if len(enabled) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## 可用技能\n用户消息提及以下技能（@技能名 或 触发词）时按需遵循；未提及则不使用：\n")
	for _, sk := range enabled {
		fmt.Fprintf(&sb, "- %s", sk.Name)
		if sk.Description != "" {
			fmt.Fprintf(&sb, "：%s", sk.Description)
		}
		if len(sk.Triggers) > 0 {
			fmt.Fprintf(&sb, "（触发词：%s）", strings.Join(sk.Triggers, "、"))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// skillSections 返回当轮注入的技能段落：索引 + 命中的技能正文。
func (a *Agent) skillSections(userInput string) []string {
	skills := a.loadSkills()
	if len(skills) == 0 {
		return nil
	}
	var secs []string
	if idx := skillIndexSection(skills); idx != "" {
		secs = append(secs, idx)
	}
	lower := strings.ToLower(userInput)
	for _, sk := range filterEnabled(skills) {
		if skillMatches(sk, lower) {
			secs = append(secs, "## 技能："+sk.Name+"\n"+sk.Body)
		}
	}
	return secs
}

// skillMatches 判断用户输入是否命中技能：显式 @名 或 任一触发词（子串、忽略大小写）。
func skillMatches(sk Skill, lowerInput string) bool {
	if lowerInput == "" {
		return false
	}
	if strings.Contains(lowerInput, "@"+strings.ToLower(sk.Name)) {
		return true
	}
	for _, t := range sk.Triggers {
		if t != "" && strings.Contains(lowerInput, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

func filterEnabled(skills []Skill) []Skill {
	var out []Skill
	for _, s := range skills {
		if s.Enabled {
			out = append(out, s)
		}
	}
	return out
}

// ListSkills 返回当前工作区全部技能（REST 用）。
func (a *Agent) ListSkills() []Skill { return a.loadSkills() }

// skillFileRe 仅供 parseSkillFile 文档引用保留（无运行时用途）。
var _ = regexp.MustCompile
