package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/store"
)

// 记忆段注入格式：有记忆生成段落，无记忆返回空。
func TestMemorySection(t *testing.T) {
	if MemorySection(nil) != "" {
		t.Error("无记忆时应返回空串")
	}
	sec := MemorySection([]store.MemoryRow{{Content: "偏好中文回复"}, {Content: "项目用 Go"}})
	if !strings.Contains(sec, "## 用户记忆") ||
		!strings.Contains(sec, "- 偏好中文回复") ||
		!strings.Contains(sec, "- 项目用 Go") {
		t.Errorf("记忆段格式不正确: %q", sec)
	}
}

// 项目级记忆：AGENTS.md / CLAUDE.md 存在即注入；workDir 为空不扫描。
func TestProjectMemorySection(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{workDir: dir}

	if a.projectMemorySection() != "" {
		t.Error("无项目文件时应返回空串")
	}

	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("规范：使用 tabs"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("构建命令 make build"), 0o644); err != nil {
		t.Fatal(err)
	}
	sec := a.projectMemorySection()
	if !strings.Contains(sec, "## 项目说明") ||
		!strings.Contains(sec, "### AGENTS.md") ||
		!strings.Contains(sec, "使用 tabs") ||
		!strings.Contains(sec, "### CLAUDE.md") ||
		!strings.Contains(sec, "make build") {
		t.Errorf("项目说明段格式不正确: %q", sec)
	}

	// 未选工作区：不扫描
	a2 := &Agent{workDir: ""}
	if a2.projectMemorySection() != "" {
		t.Error("workDir 为空时不应注入项目说明")
	}
}

// 注入必须限幅：一份超长的 AGENTS.md 不能把「提示词 + 工具定义」顶过上下文预算，
// 否则会话开局即报「开销已占满预算」；内容与已注入文件相同的副本不重复占位。
func TestProjectMemorySectionCapsAndDedupes(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{workDir: dir}

	unit := "很长的项目约定内容。"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte(strings.Repeat(unit, projectDocMaxRunes)), 0o644); err != nil {
		t.Fatal(err)
	}
	// CLAUDE.md 是 AGENTS.md 的副本（仅首尾空白不同）：去重后不应再出现。
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"),
		[]byte("  \n"+strings.Repeat(unit, projectDocMaxRunes)+"\n "), 0o644); err != nil {
		t.Fatal(err)
	}

	sec := a.projectMemorySection()
	if n := len([]rune(sec)); n > projectDocMaxRunes+600 {
		t.Errorf("注入未限幅，注入段 %d 字（上限 %d）", n, projectDocMaxRunes)
	}
	if !strings.Contains(sec, "已截断") {
		t.Error("超长文件应标注已截断，并告诉模型去哪读全文")
	}
	if strings.Contains(sec, "### CLAUDE.md") {
		t.Error("内容与 AGENTS.md 相同的文件不应重复注入")
	}

	// 内容不同时要都在（AGENTS 在前）。
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("只跑 make test"), 0o644); err != nil {
		t.Fatal(err)
	}
	sec = a.projectMemorySection()
	if !strings.Contains(sec, "### AGENTS.md") || !strings.Contains(sec, "### CLAUDE.md") {
		t.Errorf("两份不同的说明文件都应注入: %s", sec[:min(300, len(sec))])
	}
	if strings.Index(sec, "### AGENTS.md") > strings.Index(sec, "### CLAUDE.md") {
		t.Error("AGENTS.md 应排在 CLAUDE.md 之前")
	}
}

// SKILL.md 解析：frontmatter 字段 + 正文 + enabled 开关。
func TestParseSkill(t *testing.T) {
	text := `---
name: code-review
display_name: 代码审查
description: 按项目规范审查代码
triggers: 审查, code review
enabled: false
---
请按以下步骤审查：
1. 看命名
2. 看测试`
	s := parseSkill(text)
	if s.Name != "code-review" || s.Description != "按项目规范审查代码" {
		t.Errorf("frontmatter 解析不正确: %+v", s)
	}
	if s.DisplayName != "代码审查" {
		t.Errorf("display_name 解析不正确: %q", s.DisplayName)
	}
	if len(s.Triggers) != 2 || s.Triggers[0] != "审查" || s.Triggers[1] != "code review" {
		t.Errorf("触发词解析不正确: %v", s.Triggers)
	}
	if s.Enabled {
		t.Error("enabled: false 应被解析")
	}
	if !strings.Contains(s.Body, "看命名") {
		t.Errorf("正文丢失: %q", s.Body)
	}
}

// 技能目录扫描 + 触发匹配 + enabled 过滤。
func TestSkillSections(t *testing.T) {
	dir := t.TempDir()
	skillsDir := filepath.Join(dir, ".codeforge", "skills", "deploy")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sk := `---
name: deploy
description: 部署到测试环境
triggers: 部署, deploy
---
先跑测试再部署。`
	if err := os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(sk), 0o644); err != nil {
		t.Fatal(err)
	}
	// 禁用的技能：不该出现在索引与命中
	disabledDir := filepath.Join(dir, ".codeforge", "skills", "off")
	if err := os.MkdirAll(disabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	off := "---\nname: off\ndescription: 已禁用\nenabled: false\n---\n内容"
	if err := os.WriteFile(filepath.Join(disabledDir, "SKILL.md"), []byte(off), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &Agent{workDir: dir}

	// 无关输入：只有索引段（不含 off 技能）
	secs := a.skillSections("今天天气如何")
	if len(secs) != 1 || !strings.Contains(secs[0], "deploy") || strings.Contains(secs[0], "已禁用") {
		t.Errorf("索引段不正确: %v", secs)
	}

	// 命中触发词：正文注入
	secs = a.skillSections("帮我部署一下服务")
	if len(secs) != 2 || !strings.Contains(secs[1], "先跑测试再部署") {
		t.Errorf("触发注入不正确: %v", secs)
	}

	// 显式 @名 注入
	secs = a.skillSections("请按 @deploy 流程来")
	if len(secs) != 2 || !strings.Contains(secs[1], "先跑测试再部署") {
		t.Errorf("@提及注入不正确: %v", secs)
	}

	// 空工作区：无技能
	a2 := &Agent{workDir: filepath.Join(dir, "nowhere")}
	if secs := a2.skillSections("部署"); len(secs) != 0 {
		t.Errorf("无技能目录不应有段落，实际 %d 段", len(secs))
	}
}
