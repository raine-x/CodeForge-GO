// builtin_plugins.go 实现内置插件（进程内、可开关的增强能力）。
//
// 与 MCP 插件的区别：内置插件不需要外部进程，能力以「System Prompt 注入 +
// 专用工具」的形式提供；AI 在对话中根据调用时机动态决定是否使用，调用即
// 在工作流中留下「使用插件 XXX」的痕迹（前端按工具名渲染）。
//
// 插件三元信息：名称（Name）、调用时机（WhenToUse）、使用说明（Instructions）。
package agent

import (
	"fmt"
	"strings"

	"codeforge/pkg/tools"
)

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

// BuiltinGoalMode 是「目标模式」插件：让智能体在宣布完成之前，由一个独立的
// 审查者真的把效果跑出来看，判定不通过就带着证据回来。
//
// 与 Plan 的关键差别：Plan 是**常驻注入**（开关一开，模型任何时候都能看到），
// 目标模式**只在用户输入 @goal_mode 之后注入**（见 goalPluginSection）。
// 理由：目标模式是强约束 —— 「目标未验证通过就不许宣布完成」——
// 若常驻挂着，模型会在与验证无关的闲聊/小任务里也去走这套流程。
var BuiltinGoalMode = BuiltinPlugin{
	ID:      "goal_mode",
	Name:    "目标模式",
	Purpose: "在宣布完成之前由独立审查者真的把改动跑起来验证，判定不通过就带着证据回来（@goal_mode 触发）",
	WhenToUse: "只有用户输入了 @goal_mode（或 @目标模式）时才使用；没有这个触发词就不要调用目标验证。" +
		"触发后：动手改完必须验证一次，没通过就按发现清单修完再验，直到验证通过或预算用尽",
	Instructions: "使用步骤：" +
		"1) 先把「达成即算完成」写成一句可判定的话（要具体到能观察），作为验证目标；" +
		"2) 动手改，不要先宣布完成；" +
		"3) 调用目标验证工具，由独立审查者实际运行改动并给出带证据的结论；" +
		"4) 若未通过：按返回的发现清单修复，再验证一次（预算有限，别原地打转）；" +
		"5) 验证通过后才可如实告知用户结果，并附上审查者的证据；" +
		"6) 预算用尽仍未通过：停止自循环，把问题与证据交回用户，说明需要人工介入。",
}

// 内容来自对本地 prompts 目录中多类编码代理/研究工具提示词的抽象归纳，
// 不复制第三方提示词原文：保留规划、证据链、工具边界与复核习惯。
var BuiltinVulnerabilityResearch = BuiltinPlugin{
	ID:      "vulnerability_research",
	Name:    "漏洞挖掘",
	Purpose: "在授权范围内主动发现、验证和分级应用与系统漏洞",
	WhenToUse: "用户输入 @vuln_hunt、@security_audit 或 @漏洞挖掘，且目标属于用户拥有或明确授权的范围时使用；" +
		"未给出范围时先要求目标、授权、测试窗口和禁止动作",
	Instructions: "先锁定授权边界、资产清单、测试窗口、速率限制和禁止动作。可以更主动地进行端点枚举、" +
		"协议探测、输入变异、模糊测试、越权验证和最小化利用证明，但必须控制并发、速率和数据量，" +
		"不得删除或篡改数据、造成拒绝服务、持久化、横向移动、窃取凭据、外传数据或规避检测。" +
		"优先使用无害载荷和隔离账户；需要可能影响可用性的验证时先说明影响并取得确认。" +
		"每个发现都记录入口、前置条件、请求与响应证据、影响、复现步骤、修复建议和复测结果；" +
		"区分事实、推断和未验证项，不能把扫描器告警直接当成漏洞结论。",
}

var BuiltinReverseAnalysis = BuiltinPlugin{
	ID:      "reverse_analysis",
	Name:    "逆向分析",
	Purpose: "对本地授权样本进行深入静态分析、调试、反汇编和动态行为研究",
	WhenToUse: "用户输入 @reverse_analysis 或 @逆向分析，且样本位于本地或用户明确授权的分析环境中时使用；" +
		"来源、授权或隔离条件不明确时先暂停执行",
	Instructions: "可以更深入地分析本地样本：计算哈希、识别格式与架构、提取字符串和配置、反汇编/反编译、" +
		"调试、跟踪系统调用、构造输入、运行样本并观察行为。运行前记录样本来源与哈希，" +
		"优先使用无网络或隔离沙箱、快照和一次性账户；不得让样本接触真实凭据、生产数据或未授权网络。" +
		"禁止持久化、横向移动、凭据窃取、数据外传、破坏宿主机或为绕过防护而提供可直接滥用的部署步骤；" +
		"必要的本地调试与脱壳应限于用户控制的样本和环境。结论必须关联函数、输入、状态、输出等证据，" +
		"明确标注置信度，不把反编译伪代码当作事实。报告包含样本标识、方法、关键路径、行为、局限和下一步。",
}

// builtinPlugins 列出全部内置插件定义（新增内置插件时在此追加）。
func builtinPlugins() []BuiltinPlugin {
	return []BuiltinPlugin{BuiltinSkillCreator, BuiltinMultiAgent, BuiltinPlan, BuiltinGoalMode, BuiltinVulnerabilityResearch, BuiltinReverseAnalysis}
}

// ListBuiltinPlugins 导出全部内置插件定义（设置页列表用）。
func ListBuiltinPlugins() []BuiltinPlugin { return builtinPlugins() }

// SetSkillCreatorEnabled 开关 Skill Creator（来自配置 builtin_plugins.skill_creator）。
// 开：systemPrompt 注入插件说明 + create_skill 工具可用（main.go 负责注册工具）；
// 关：两者都没有，模型无从使用。
func (a *Agent) SetSkillCreatorEnabled(on bool) {
	a.setBuiltinOn(BuiltinSkillCreator.ID, on)
}

// setBuiltinOn 是启用表的唯一写入口（见 Agent.rtMu）。
//
// ⚠️ 必须持锁：设置页切插件开关走的是 HTTP 处理器 goroutine，而运行中的循环
// 正在 builtinPluginSection 里读这张 map —— 裸读写是
// `fatal error: concurrent map read and map write`，整个进程直接死。
func (a *Agent) setBuiltinOn(id string, on bool) {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	if a.builtinOn == nil {
		a.builtinOn = map[string]bool{}
	}
	a.builtinOn[id] = on
}

// builtinOnSnapshot 返回启用表的副本（读者走这里，别直接读 map）。
func (a *Agent) builtinOnSnapshot() map[string]bool {
	a.rtMu.RLock()
	defer a.rtMu.RUnlock()
	out := make(map[string]bool, len(a.builtinOn))
	for k, v := range a.builtinOn {
		out[k] = v
	}
	return out
}

// SetMultiAgentEnabled 同步多智能体插件的提示词开关；工具注册由启动层负责。
func (a *Agent) SetMultiAgentEnabled(on bool) {
	a.setBuiltinOn(BuiltinMultiAgent.ID, on)
}

// SetPlanEnabled 同步计划模式插件的提示词开关（不注册工具，纯 System Prompt 注入）。
func (a *Agent) SetPlanEnabled(on bool) {
	a.setBuiltinOn(BuiltinPlan.ID, on)
}

// SetGoalModeEnabled 同步目标模式插件的提示词开关（工具注册由启动层负责）。
func (a *Agent) SetGoalModeEnabled(on bool) {
	a.setBuiltinOn(BuiltinGoalMode.ID, on)
}

// SetVulnerabilityResearchEnabled 同步漏洞挖掘插件的提示词开关。
func (a *Agent) SetVulnerabilityResearchEnabled(on bool) {
	a.setBuiltinOn(BuiltinVulnerabilityResearch.ID, on)
}

// SetReverseAnalysisEnabled 同步逆向分析插件的提示词开关。
func (a *Agent) SetReverseAnalysisEnabled(on bool) {
	a.setBuiltinOn(BuiltinReverseAnalysis.ID, on)
}

// GoalLedgerFor 返回某会话的目标账本。
//
// 由服务端在每轮 run 的 ctx 上装（tools.WithGoalLedger），装的必须是**本轮那个
// 会话**的账本 —— 记到别处的话，注入的提示段永远为空，
// 「目标未通过就不许宣布完成」这条约束就静默失效了。
func (a *Agent) GoalLedgerFor(sessionID string) (GoalLedger, bool) {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return nil, false
	}
	return sess, true
}

// goalPluginSection 生成本轮的目标模式注入段（空串 = 本轮不注入）。
//
// 注入条件 = 插件开关开 **且**（本轮输入含 @goal_mode **或** 目标已开启）。
//
// ⚠️ 两个条件缺一不可：
//   - 只看触发词 → 自循环第二轮起（输入里已没有 @goal_mode）约束就消失了，
//     而恰恰是第二轮之后模型最容易「我改好了」就直接宣布完成；
//   - 只看目标开启 → 用户开过目标模式后，本会话**每一轮**（包括后续无关的闲聊）
//     都会被塞进「不许宣布完成」，太吵。
//
// 注入走 systemPromptFor（按会话）而不是 systemPrompt：账本是按会话的，
// 而 systemPrompt 拿不到 sess。
func (a *Agent) goalPluginSection(lastInput string, sess *Session) string {
	if !a.builtinOnSnapshot()[BuiltinGoalMode.ID] {
		return ""
	}
	if sess == nil {
		return ""
	}
	goal := sess.Goal()
	triggered := tools.GoalTriggered(lastInput)
	if !triggered && !goal.Open() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"## 内置插件：%s\n- 能力：%s\n- 调用时机：%s\n- %s\n"+
			"- 工作流提醒：一旦决定使用该插件，先在回复中说明「使用插件 %s」，再调用其工具；"+
			"不要在不符合调用时机时调用。\n",
		BuiltinGoalMode.Name, BuiltinGoalMode.Purpose, BuiltinGoalMode.WhenToUse,
		BuiltinGoalMode.Instructions, BuiltinGoalMode.Name))
	// 目标开着就把账本状态一并注入：轮次与剩余额度必须**每步可见**，
	// 否则模型会在同一状态下反复自我确认，或在预算用尽后继续硬撑。
	if sec := goal.PromptSection(); sec != "" {
		sb.WriteString("\n" + sec)
	}
	return sb.String()
}

// triggeredPluginSection 只在用户明确使用对应触发词时注入，避免高风险
// 操作指南常驻到普通对话；插件开关关闭时即使输入带触发词也不生效。
func (a *Agent) triggeredPluginSection(p BuiltinPlugin, lastInput string) string {
	if !a.builtinOnSnapshot()[p.ID] || !pluginTriggered(p.ID, lastInput) {
		return ""
	}
	return fmt.Sprintf(
		"## 内置插件：%s\n- 能力：%s\n- 调用时机：%s\n- %s\n"+
			"- 工作流提醒：先确认授权范围和禁止动作，再开始主动操作；报告中区分事实、推断和未验证项。\n",
		p.Name, p.Purpose, p.WhenToUse, p.Instructions)
}

func pluginTriggered(id, input string) bool {
	lower := strings.ToLower(input)
	var tokens []string
	switch id {
	case BuiltinVulnerabilityResearch.ID:
		tokens = []string{"@vuln_hunt", "@security_audit", "@漏洞挖掘"}
	case BuiltinReverseAnalysis.ID:
		tokens = []string{"@reverse_analysis", "@逆向分析"}
	}
	for _, token := range tokens {
		if strings.Contains(lower, strings.ToLower(token)) {
			return true
		}
	}
	return false
}

// builtinPluginSection 生成启用的内置插件注入段（含使用约定与工作流提醒要求）。
//
// 读启用表走快照：设置页随时可能改它（见 setBuiltinOn）。
func (a *Agent) builtinPluginSection() string {
	enabled := a.builtinOnSnapshot()
	var sb string
	for _, p := range builtinPlugins() {
		if p.ID == BuiltinVulnerabilityResearch.ID || p.ID == BuiltinReverseAnalysis.ID {
			continue // 高风险研究指南按 @ 触发，不常驻注入。
		}
		if !enabled[p.ID] {
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
