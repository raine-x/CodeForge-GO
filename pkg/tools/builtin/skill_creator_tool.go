// skill_creator_tool.go 提供 create_skill 工具（内置插件 Skill Creator 的能力）：
// 让 AI 在对话中动态创建/更新工作区技能（.codeforge/skills/<name>/SKILL.md）。
package builtin

import (
	"context"
	"encoding/json"
	"errors"
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

// Metadata 声明副作用等级。
func (t *SkillCreatorTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectWrite}
}

// Name 实现 tools.Tool。
func (t *SkillCreatorTool) Name() string { return "create_skill" }

// Description 实现 tools.Tool。
func (t *SkillCreatorTool) Description() string {
	return "创建或更新工作区技能（.codeforge/skills/<name>/SKILL.md）。仅在用户明确要求创建/沉淀/改进技能时使用；" +
		"写入后技能即刻生效，可通过 @技能名 或触发词唤起。同名技能会被覆盖（即迭代）。" +
		"⚠️ 更新一个**已存在**的技能前，必须先 read_file 读 .codeforge/skills/<name>/SKILL.md —— " +
		"「先读后写」是硬约束，没读过会被直接拒绝（新建技能不需要读）。"
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
func (t *SkillCreatorTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	// 守卫顺序与其它文件工具一致：先确认选了工作区，再解析路径。
	//
	// 缺这一句的后果很具体：Root() 为空时
	// filepath.Join("", ".codeforge", …) 是**相对路径**，
	// 于是技能被写进**服务进程的工作目录** —— 用户在界面上看不到，
	// 却在后续对话里生效（可通过 @技能名 唤起）。
	if t.fs.noWorkspace() != nil {
		return tools.Err("未选择工作区：请先在界面点击「选择工作区」后再创建技能"), nil
	}

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

	// 路径经 ResolveChecked 过围栏。skillNameRe 已经挡住了名字里的路径穿越，
	// 但这一段仍必须走围栏：工作区根自身可能是软链指向别处
	//（macOS 的 /var → /private/var，或用户自己 ln -s），
	// 硬编码的 .codeforge 会跟着落到围栏之外。
	dir, err := t.fs.ResolveChecked(filepath.Join(".codeforge", "skills", p.Name))
	if err != nil {
		return tools.Err("技能目录超出工作区: %v", err), nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.Err("创建技能目录失败: %v", err), nil
	}
	file := filepath.Join(dir, "SKILL.md")

	// action 必须在写之前判定：写入成功后文件必然存在，再 Stat 无法区分
	// 「新建」与「覆盖」。这里靠快照栈里有没有该路径来判断 ——
	// 之前写过才有快照。
	action := "已创建技能"
	for _, s := range t.fs.Snapshots() {
		if s.Path == file {
			action = "已更新技能" // 同名覆盖 = 迭代
			break
		}
	}

	if err := t.skillWrite(ctx, file, []byte(sb.String())); err != nil {
		var stale *tools.ErrStaleContent
		if errors.As(err, &stale) {
			return tools.Err("技能文件 %s 已被外部修改，或本会话还没读取过它。"+
				"请先 read_file 读取后再更新，或换一个技能名。", file), nil
		}
		return tools.Err("写入 SKILL.md 失败: %v", err), nil
	}

	return tools.Ok(fmt.Sprintf("%s「%s」，用户可通过 @%s 或触发词唤起。", action, p.Name, p.Name)), nil
}

// skillWrite 把技能正文写进 SKILL.md。
//
// 三件事必须在这里做完，缺一件就出问题：
//
//  1. 围栏：路径已由调用方 ResolveChecked 校验。
//  2. 撤销：走 casWrite + snapshot 才能压栈，用户点「撤销」才撤得掉。
//     技能写入后会立刻影响后续所有对话（@技能名 可唤起），
//     撤不掉是有实际后果的。
//  3. 原子：casWrite 内部落盘走 atomicWriteFile。裸 os.WriteFile 是
//     「打开→截断→写」，写一半被杀会留半截 SKILL.md，
//     而半截的技能文件会被当成合法技能加载。
//
// 顺序照 WriteFileTool：casWrite 返回写前内容 → 用它压快照 → 刷指纹。
// 三个动作缺一不可，少刷指纹会导致「创建 → 更新 → 更新」第二次被判成
// 「被外部修改」而永久锁死。
//
// 单独拆成函数是为了让「不许有裸 os.WriteFile / os.Stat / os.MkdirAll」
// 这条纪律能被 AST 断言稳定检查（见 skill_creator_guard_test.go）。
func (t *SkillCreatorTool) skillWrite(ctx context.Context, path string, data []byte) error {
	existed := false
	if _, err := os.Stat(path); err == nil {
		existed = true
	} else if !os.IsNotExist(err) {
		return err
	}

	before, err := t.fs.casWrite(ctx, path, existed, data)
	if err != nil {
		return err
	}
	t.fs.snapshot(ctx, path, before, existed)
	t.fs.markReadSeen(ctx, path, data)
	return nil
}

// RegisterSkillCreator 注册 Skill Creator 插件的工具（仅在配置启用时调用）。
func RegisterSkillCreator(reg *tools.Registry, fs *FS) {
	reg.Register(NewSkillCreatorTool(fs))
}
