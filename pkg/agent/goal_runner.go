// goal_runner.go 实现目标模式的**审查者**（GoalVerifier）。
//
// 为什么必须是独立子智能体，而不是主智能体自问自答：
// 同一个脑子刚写完代码，让它自己判，它只会确认你 —— 这正是本功能要消灭的病。
// 独立 judge + 只读工具 + 专用提示词，是自审查成立的前提。
//
// 三条硬约束（都有测试锁住，见 goal_runner_test.go）：
//  1. 工具白名单**不含任何写工具** —— 它只判定，不修复。修复权在主智能体
//     （有完整上下文、用户能随时打断的那一层）。
//  2. 白名单**不含 goal_verify 与 delegate_subagents** —— 防递归套娃。
//     这是唯一真正生效的护栏，比 ctx 里的深度计数器可靠（计数器会被人绕过）。
//  3. 传**真 audit logger**（子智能体那套传的是 nil）—— 审查者会自主执行命令，
//     不能没有痕迹。
package agent

import (
	"context"
	"fmt"
	"strings"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
)

// GoalVerifierID 是审查者在审批卡与事件里显示的身份。
//
// 走 tools.SubagentScope.TaskID 透传，于是用户看到的是「子任务 goal-verify · 验证」
// 而不是一堆分不清来源的命令审批。
const GoalVerifierID = "goal-verify"

// GoalVerifierMode 是审查者在审批卡与进度卡上的模式标签。
const GoalVerifierMode = "verify"

// goalVerifierTools 是审查者的工具白名单。
//
// ⚠️ 三类工具刻意**不在**其中，理由见文件头：
//   - 写类（write_file / edit_file / delete_file）：它只判定，不修复
//   - goal_verify：防递归套娃
//   - delegate_subagents / create_skill：它不是主智能体，也不该有那些能力
//
// run_command 在白名单里是刻意的：verify 的定义就是「把程序跑起来看」。
// 只读代码猜对错是本功能要消灭的**假验证**，不是验证。
var goalVerifierTools = []string{
	"read_file",
	"list_dir",
	"search_files",
	"run_command",
}

// goalVerifierToolSet 返回白名单查表。
func goalVerifierToolSet() map[string]bool {
	set := make(map[string]bool, len(goalVerifierTools))
	for _, n := range goalVerifierTools {
		set[n] = true
	}
	return set
}

// GoalVerifier 跑一轮审查：派生一个独立 judge 子智能体，把它的结论解析成判定。
type GoalVerifier struct {
	parent *Agent
}

// NewGoalVerifier 创建审查者调度器。
func NewGoalVerifier(parent *Agent) *GoalVerifier {
	return &GoalVerifier{parent: parent}
}

// mergeUsage 把审查会话的 token 用量并入主会话。
//
// 与 SubagentRunner.mergeUsage 同口径：子会话不落盘（persist=false），
// 它的用量只能靠这一下并回去，否则界面上这一整轮是「凭空消失」的。
func (v *GoalVerifier) mergeUsage(ctx context.Context, child *Session) {
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return
	}
	parent, ok := v.parent.history.Get(sc.SessionID)
	if !ok {
		return
	}
	in, hit, out := child.UsageSnapshot()
	parent.mergeUsage(in, hit, out)
}

// Verify 跑一轮审查。
//
// ctx 会被注入 SubagentScope（TaskID=goal-verify）：于是审查者执行命令触发审批时，
// 审批卡能标明来源。Allowed 留空 —— 审查者要读工作区外的文件（比如跑起来的
// 服务日志），不给它加路径围栏。
//
// 不落盘：审查会话 persist=false，不进历史库，避免把大量命令输出灌进会话记录。
func (v *GoalVerifier) Verify(ctx context.Context, goal string, priorFindings string) tools.GoalVerifyOutcome {
	if v == nil || v.parent == nil {
		return tools.GoalVerifyOutcome{Verdict: tools.VerdictBlocked,
			Report: "审查者未就绪（GoalVerifier 为空）", Err: fmt.Errorf("审查者未就绪")}
	}
	workDir := v.parent.WorkDir()
	child := v.parent.newGoalVerifier(workDir)

	sess := &Session{ID: "goal-verify", Workspace: workDir}
	prompt := goalVerifierPrompt(goal, priorFindings)
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, prompt))
	sess.SetLastUserInput(prompt)

	ctx = tools.WithSubagentScope(ctx, tools.SubagentScope{
		Mode:   GoalVerifierMode,
		TaskID: GoalVerifierID,
	})

	// 实时进度复用**子智能体进度卡**（tools.SubagentEvent）。
	//
	// 为什么不用任务清单那条通道：那张卡由服务端的 todo 行驱动，借来显示验证进度
	// 会把「正在验证」混进用户真实的任务清单（`任务清单 5/6` 里凭空多一条）。
	// 而审查者本来就是个子智能体，发子智能体事件是如实描述，不是硬套。
	// 前端徽标已加 verify → 「验证」（见 ui.js 的 MODE_LABEL）。
	sink, hasSink := tools.SubagentSinkFrom(ctx)
	emitEvent := func(status, detail, summary string) {
		if !hasSink || sink == nil {
			return
		}
		sink(tools.SubagentEvent{
			ID: GoalVerifierID, Mode: GoalVerifierMode, Status: status,
			Detail: detail, Summary: summary,
		})
	}
	emitEvent("started", "开始验证目标", "")

	var report strings.Builder
	step := 0
	err := child.runLoopEphemeral(ctx, sess, func(ev Event) {
		switch ev.Type {
		case EventText, "reasoning":
			report.WriteString(ev.Text)
		case EventStep:
			step = ev.Step
		case EventToolCall:
			// 显示它正在做什么，而不是干等着 —— 这一轮可能要跑几分钟。
			emitEvent("running", "正在调用 "+ev.ToolName, "")
		}
	})
	// 审查消耗的用量计入主会话，否则这一整轮在界面上「凭空消失」。
	v.mergeUsage(ctx, sess)

	out := tools.GoalVerifyOutcome{Report: strings.TrimSpace(report.String()), Steps: step, Err: err}
	if err != nil {
		// 审查过程失败 ≠ 目标未达成，但也绝不能算通过。
		out.Verdict = tools.VerdictBlocked
		if out.Report == "" {
			out.Report = "审查过程失败：" + err.Error()
		}
		emitEvent("failed", "审查过程失败", truncateTail(err.Error(), 200))
		return out
	}
	verdict, tail := tools.ParseVerdict(out.Report)
	out.Verdict = verdict
	if tail != "" {
		// 判定行之后的内容（发现清单）单独回传，主智能体拿它当修复清单。
		out.Report = out.Report + "\n\n" + tail
	}
	// 终态：把判定结论摆到卡片上，让人一眼看到「过了」还是「没过」。
	emitEvent("completed", "判定 "+string(verdict), truncateTail(out.Report, 200))
	return out
}

// newGoalVerifier 构造审查者子智能体。
//
// 与 newSubagentIn 的差异，逐条都有理由：
//   - 工具白名单换成 goalVerifierTools（只读 + run_command）
//   - **audit 传真 logger**（子智能体传 nil）
//   - 步数上限用自己的预算，不继承子智能体策略
//   - promptOverride 换成 judge 提示词
//   - 上下文窗口必须继承（否则小窗口端点会一路堆到 12 万 token 才压缩 → 上游 400）
func (a *Agent) newGoalVerifier(workDir string) *Agent {
	registry := tools.NewRegistry()
	allowed := goalVerifierToolSet()
	for _, tool := range a.registry.List() {
		if allowed[tool.Name()] {
			registry.Register(tool)
		}
	}
	// audit 用真的：审查者会自主执行命令，那些命令必须进审计日志。
	// 走角色工厂，audit 从父执行器派生，不再手工传 —— 传错就是静默降级。
	executor := tools.NewExecutorFor(a.executor, tools.RoleGoalReviewer, registry)
	cfg := a.cfg
	cfg.MaxSteps = goalVerifierSteps(a.MaxSteps())
	child := New(cfg, a.llmCfgSnapshot(), a.providerSnapshot(), executor, a.history, workDir)
	child.memoryStore = a.memoryStore
	// 与 child.builtinOn 同一档约定：子智能体不继承插件开关，否则提示词会提到
	// 它手里根本没有的工具。
	child.builtinOn = map[string]bool{}
	child.contextWindow.Store(int64(a.ContextWindow()))
	child.SetExposure(a.exposureFn())
	// 建好即写死，之后只被自己那个 goroutine 用（见字段注释）。
	child.promptOverride = goalVerifierSystemPrompt
	return child
}

// goalVerifierToolTimeout 是审查者内部**单个工具**的兜底超时。
//
// 比默认 120s 宽：一个 run_command 可能要等编译或等服务起来。
// 注意这不是业务上限 —— 业务上限是审查者的步数预算（goalVerifierSteps）。
// 用户点停止照样立刻中断（cctx 派生自 ctx）。
//
// 实际取值由角色表给出（单一事实源），这里保留别名是为了让既有测试
// （TestGoalVerifierToolTimeoutIsWiderThanDefault）继续盯住这条约束 ——
// 角色表和调用点分家正是本项要消灭的那类漂移。
var goalVerifierToolTimeout = tools.RoleTimeout(tools.RoleGoalReviewer)

// goalVerifierSteps 解析审查者的步数预算。
//
// 与主智能体同源（继承主 loop 的 max_steps），但有硬顶：审查者步数无上限时
// 一个「起服务 → 跑命令 → 读日志」的任务可能几十步都花完，而目标模式是
// 一次调用的内部循环，失控的代价由用户承担。
func goalVerifierSteps(parent int) int {
	const cap = 30
	if parent <= 0 {
		return 15 // 主 loop 也没配（配置异常）：给个能跑的兜底值
	}
	if parent > cap {
		return cap
	}
	return parent
}

// goalVerifierSystemPrompt 是审查者的系统提示词。
//
// 措辞必须与「实际授予的工具集」一致，否则模型会反复尝试调用不存在的工具
// 直到步数耗尽（subagent.go 的 subagentPrompt 记过这个坑）。
const goalVerifierSystemPrompt = `你是 CodeForge 的**独立验证审查者**。

你不参与实现，也**不能修改任何东西**。你的唯一职责是回答一个问题：
「主智能体声称的目标，到底达成了没有？」

## 你能做什么
- 读取文件、列出目录、检索代码
- 执行命令（git diff、起服务、跑测试、curl、打接口、看日志…）

## 硬约束
- 禁止修改、创建、删除任何文件 —— 你手里没有这类工具，也不要试图绕过。
- 禁止委派子智能体、禁止创建技能。
- 禁止仅凭阅读代码就下结论。「我看了代码，逻辑没问题」不是验证 —— 必须实际运行。
- 不要为了让结论好看而放宽标准。你判 FAIL 的代价只是主智能体多修一轮；
  你误判 PASS 的代价是把坏结果发给用户。

## 验证步骤
1. **先看改了什么**。用 git diff（必要时加 --stat、比对上游）确认实际改动范围。
   如果它与「要达成的目标」对不上，这本身就是一条发现。
2. **找到可观察的界面**。用户/人或程序会在哪里碰到这段改动？
   命令行 → 敲命令；服务/API → 发请求；界面 → 真正点开（能截图就截图）；
   库 → 走它的公开入口。内部函数不算界面 —— 顺着调用方往上找，总能找到一个。
3. **把它跑起来，采集证据**。命令的真实输出、响应体、界面截图。
   证据是你唯一的产出物；你的记忆不算。
4. **顺手压一压**（至少一次）。空值、重复传参、错误路径、边界输入、相邻功能
   有没有被这次改动带坏？在同一个界面上探测，不要跳去跑单元测试充数。
5. **给出判定**。

## 判定标准
- **PASS** —— 你真的运行了它，它在该有的界面上表现符合目标，没有发现阻碍达成的问题。
- **FAIL** —— 你运行了它，它不符合目标。必须给出**可复现**的发现：
  做了什么 → 看到什么 → 为什么这说明没达成。
- **BLOCKED** —— 够不到可观察状态（构建坏了、缺依赖、服务起不来）。
  这不是对改动的判决，但同样不算通过。
- **SKIP** —— 根本没有运行时可观察面（纯文档、纯类型声明、纯测试改动）。一句话说明即可。

**存疑即 FAIL。** 证据不足时不要给 PASS。

## 输出格式
先给判定前的证据部分（你做了什么、看到什么，带上真实输出），再在**最后一行**给出判定，
格式严格如下（这一行会被程序解析，务必照写）：

VERDICT: PASS | FAIL | BLOCKED | SKIP

FAIL 时把发现整理成清单，每条写成：
- 【界面/命令】做了什么 → 实际看到什么 → 说明什么问题`

// goalVerifierPrompt 生成审查者这一轮的任务提示词。
func goalVerifierPrompt(goal, priorFindings string) string {
	var sb strings.Builder
	sb.WriteString("请验证以下目标是否已经达成。\n\n## 目标\n" + strings.TrimSpace(goal) + "\n")
	if f := strings.TrimSpace(priorFindings); f != "" {
		// 带上上一轮的发现：主智能体可能只修了一部分，
		// 审查者要重点确认「上次说的那些问题现在还在不在」。
		sb.WriteString("\n## 上一轮你报告的问题（请确认是否已修复，并检查是否有新问题）\n" + f + "\n")
	}
	sb.WriteString("\n请先确认实际改了什么，再把它跑起来看效果，最后给出带证据的判定。")
	return sb.String()
}

// GoalMaxRounds 归一化配置里的轮数（供工具层调用，避免重复实现）。
func GoalMaxRounds(cfg config.BuiltinPluginsConfig) int { return cfg.GoalModeRounds() }
