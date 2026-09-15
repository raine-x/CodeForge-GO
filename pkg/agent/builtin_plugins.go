// builtin_plugins.go 实现内置插件（进程内、可开关的增强能力）。
//
// 与 MCP 插件的区别：内置插件不需要外部进程，能力以「System Prompt 注入 +
// 专用工具」的形式提供；AI 在对话中根据调用时机动态决定是否使用，调用即
// 在工作流中留下「使用插件 XXX」的痕迹（前端按工具名渲染）。
//
// 插件三元信息：名称（Name）、调用时机（WhenToUse）、使用说明（Instructions）。
package agent

import "fmt"

// BuiltinPlugin 描述一个内置插件。
type BuiltinPlugin struct {
	ID           string // 稳定标识（配置开关 key、工具注册依据）
	Name         string // 展示名
	Purpose      string // 用途（一句话）
	WhenToUse    string // 调用时机（何时该用 / 何时不该用）
	Instructions string // 使用说明（注入给模型的操作步骤）
}

// BuiltinSkillCreator 是「Skill Creator」插件：让 AI 沉淀可复用技能。
var BuiltinSkillCreator = BuiltinPlugin{
	ID:        "skill_creator",
	Name:      "Skill Creator",
	Purpose:   "创建、修改与迭代本工作区的技能（.codeforge/skills/<名称>/SKILL.md），把可复用的做法沉淀下来",
	WhenToUse: "当用户要求「创建一个技能 / 把这套流程保存下来以后复用 / 改进某个技能」时使用；一次性任务或普通问答不要使用",
	Instructions: "使用步骤：" +
		"1) 与用户确认技能名（英文 slug）与触发词；" +
		"2) 使用技能创建工具写入 SKILL.md（正文写清操作步骤与约定，让未来的模型能独立照做）；" +
		"3) 完成后告知用户技能名与唤起方式（@技能名 或命中触发词）。",
}

// BuiltinMultiAgent 是多智能体协作插件：将真正独立的探索/实现子任务并行委派。
var BuiltinMultiAgent = BuiltinPlugin{
	ID:        "multi_agent",
	Name:      "Multi-Agent",
	Purpose:   "把互不重叠的代码探索或实现拆给最多 5 个子智能体并行处理",
	WhenToUse: "只有任务包含多个独立部分、代码范围不重叠，或需要并行只读探索时使用；单文件连续修改、共享状态任务不要使用",
	Instructions: "使用步骤：" +
		"1) 先拆分任务并为每个子任务声明唯一 id、mode（explore/implement）和 paths；" +
		"2) 确认不同子任务 paths 不重叠；implement 必须声明 paths，explore 只能读文件/列目录/搜索；" +
		"3) 使用多智能体委派能力，最多 5 个；" +
		"4) 汇总子任务结果后再由主智能体做跨范围整合与最终验证。不要为了并行而拆分单一连续操作。",
}

// BuiltinPlan 是「计划模式」插件：用户 @plan（或要求方案设计）时，AI 只读探索
// 后输出可执行的结构化项目计划书。与 Skill Creator/Multi-Agent 不同，它不需要
// 专属工具 —— 探索用已有只读工具完成，产物是计划书文本。常驻注入 WhenToUse
// 让模型在收到 @plan 时立即进入计划模式。
var BuiltinPlan = BuiltinPlugin{
	ID:        "plan",
	Name:      "Plan（计划模式）",
	Purpose:   "针对问题产出可执行的结构化项目计划书（@plan 触发，只读规划，不改任何文件）",
	WhenToUse: "当用户输入 @plan、或要求「出方案 / 做计划 / 写实现方案 / 计划书」时使用；只读产出计划书，不执行修改",
	Instructions: "只读规划模式：本轮禁止创建/修改/删除任何文件，禁止运行改变系统状态的命令；" +
		"探索用只读能力（读取文件、列目录、检索代码、查看版本历史）。" +
		"流程：理解需求 → 彻底探索（读关键文件、找既有模式与调用方、追踪代码路径）→ 设计方案（说明权衡）→ 拆分步骤。" +
		"输出 Markdown 计划书：目标 / 现状与关键文件（文件:行号）/ 实现方案 / 实施步骤（标注依赖与改动文件）/ 风险与验证。" +
		"计划书必须具体到可直接照做。若输入是普通对话而非计划任务，回复简短说明不需要计划即可，不要强套格式。",
}

// builtinPlugins 列出全部内置插件定义（新增内置插件时在此追加）。
func builtinPlugins() []BuiltinPlugin {
	return []BuiltinPlugin{BuiltinSkillCreator, BuiltinMultiAgent, BuiltinPlan}
}

// ListBuiltinPlugins 导出全部内置插件定义（设置页列表用）。
func ListBuiltinPlugins() []BuiltinPlugin { return builtinPlugins() }

// SetSkillCreatorEnabled 开关 Skill Creator（来自配置 builtin_plugins.skill_creator）。
// 开：systemPrompt 注入插件说明 + create_skill 工具可用（main.go 负责注册工具）；
// 关：两者都没有，模型无从使用。
func (a *Agent) SetSkillCreatorEnabled(on bool) {
	if a.builtinOn == nil {
		a.builtinOn = map[string]bool{}
	}
	a.builtinOn[BuiltinSkillCreator.ID] = on
}

// SetMultiAgentEnabled 同步多智能体插件的提示词开关；工具注册由启动层负责。
func (a *Agent) SetMultiAgentEnabled(on bool) {
	if a.builtinOn == nil {
		a.builtinOn = map[string]bool{}
	}
	a.builtinOn[BuiltinMultiAgent.ID] = on
}

// SetPlanEnabled 同步计划模式插件的提示词开关（不注册工具，纯 System Prompt 注入）。
func (a *Agent) SetPlanEnabled(on bool) {
	if a.builtinOn == nil {
		a.builtinOn = map[string]bool{}
	}
	a.builtinOn[BuiltinPlan.ID] = on
}

// builtinPluginSection 生成启用的内置插件注入段（含使用约定与工作流提醒要求）。
func (a *Agent) builtinPluginSection() string {
	var sb string
	for _, p := range builtinPlugins() {
		if !a.builtinOn[p.ID] {
			continue
		}
		sb += fmt.Sprintf(
			"## 内置插件：%s\n"+
				"- 能力：%s\n"+
				"- 调用时机：%s\n"+
				"- %s\n"+
				"- 工作流提醒：一旦决定使用该插件，先在回复中说明「使用插件 %s」，再调用其工具；"+
				"不要在不符合调用时机时调用。\n\n",
			p.Name, p.Purpose, p.WhenToUse, p.Instructions, p.Name)
	}
	return sb
}
