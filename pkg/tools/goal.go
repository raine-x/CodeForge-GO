// goal.go 存放目标模式（Goal Mode）的**数据类型与判定解析**。
//
// ⚠️ 为什么这些类型住在 pkg/tools 而不是 pkg/agent：
// 账本挂在 Agent 的 Session 上（读写方法在 pkg/agent/goal.go），但 goal_verify 工具
// 在 pkg/tools/builtin 里，它必须能操作同一个账本。而 pkg/agent 的**同包测试**
// 已经 import 了 builtin（diffstats_test.go），若再让 builtin 反向 import agent
// 就成了环。
//
// 于是把纯数据（判定结论、账本快照、VERDICT 解析、账本接口与 ctx 槽位）放在
// 依赖树最底层的 pkg/tools：agent 与 builtin 都往下引用，方向一致、无环。
// agent 侧用类型别名（见 pkg/agent/goal.go）保持既有写法不变。
package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 判定结论
// ---------------------------------------------------------------------------

// GoalVerdict 是审查者对「目标是否达成」的判定。
//
// 只有 Pass 才允许关闭目标。其余三种一律**保持目标开启** —— 偏保守是刻意的：
// 误判 PASS 会把坏结果发出去，误判 FAIL 只是多让人看一眼。
type GoalVerdict string

const (
	// VerdictPass 目标达成，关闭账本。
	VerdictPass GoalVerdict = "PASS"
	// VerdictFail 明确未达成，带着发现清单回来。
	VerdictFail GoalVerdict = "FAIL"
	// VerdictBlocked 够不到可观察的状态（构建坏了、缺依赖、服务起不来）。
	// 这**不是**对改动的判决 —— 但同样不能算通过。
	VerdictBlocked GoalVerdict = "BLOCKED"
	// VerdictSkip 根本没有运行时可观察面（纯文档、纯类型声明）。
	VerdictSkip GoalVerdict = "SKIP"
)

// Passes 只有 Pass 返回 true。
func (v GoalVerdict) Passes() bool { return v == VerdictPass }

// ---------------------------------------------------------------------------
// 目标账本
// ---------------------------------------------------------------------------

// GoalState 是一个会话上的目标账本快照。
//
// ⚠️ 值语义（不是指针）：调用方拿到的是一份快照，可以随便读；
// 所有修改都走账本实现（Session 上的方法），避免共享可变状态。
type GoalState struct {
	// Goal 目标原文（智能体自己复述的「达成即算完成」）。
	Goal string
	// Round 已经进行到第几轮验证（首次验证后为 1）。
	Round int
	// MaxRounds 本次预算上限，来自配置（夹在 [1, GoalModeMaxRoundsCap]）。
	MaxRounds int
	// LastVerdict 最近一次判定。
	LastVerdict GoalVerdict
	// LastFindings 最近一次带回来的发现（人类可读，可能为空）。
	LastFindings string
	// LastStep 上次验证发生在会话的第几步（用于注入提示时说明进展）。
	LastStep int
	// VerifiedAt 上次验证的时刻。
	VerifiedAt time.Time
}

// Open 返回是否有未达成的目标。
func (g GoalState) Open() bool { return strings.TrimSpace(g.Goal) != "" }

// Exhausted 返回预算是否已用尽。
//
// ⚠️ 用 >= 而不是 ==：MaxRounds 可能被配置调小（运行中被改），
// 已经用掉的轮数不会倒退，这时必须判定为用尽而不是继续放行。
func (g GoalState) Exhausted() bool {
	return g.MaxRounds > 0 && g.Round >= g.MaxRounds
}

// Remaining 返回还能验几轮（不会为负）。
func (g GoalState) Remaining() int {
	if g.MaxRounds <= 0 {
		return 0
	}
	if r := g.MaxRounds - g.Round; r > 0 {
		return r
	}
	return 0
}

// Clone 返回副本（时间戳一并复制）。
func (g GoalState) Clone() GoalState { return g }

// PromptSection 渲染注入给主智能体的账本提示段（空串 = 没有开启的目标）。
//
// 为什么要注入而不是靠模型自己记得：模型看不到「我已经验过两轮了」这个事实，
// 除非我们把它写进每一步的上下文。轮次与剩余额度必须每步可见，
// 否则它会在同一状态下反复自我确认，或者在预算用尽后继续硬撑。
func (g GoalState) PromptSection() string {
	if !g.Open() {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("## 当前目标（目标模式）\n")
	sb.WriteString("- 目标：" + g.Goal + "\n")
	if g.Round == 0 {
		sb.WriteString("- 状态：**尚未验证**。动手改完（或确认已达成）后必须调用目标验证，" +
			"不能仅凭「我改完了」宣布完成。\n")
	} else {
		sb.WriteString(fmt.Sprintf("- 状态：第 %d/%d 轮验证未通过", g.Round, g.MaxRounds))
		if g.VerifiedAt.Year() > 0 {
			sb.WriteString("（上次验证 " + g.VerifiedAt.Format("15:04:05") + "）")
		}
		sb.WriteString("\n")
	}
	if g.LastVerdict != "" && !g.LastVerdict.Passes() {
		sb.WriteString("- 上次判定：" + string(g.LastVerdict) + "\n")
	}
	if f := strings.TrimSpace(g.LastFindings); f != "" {
		sb.WriteString("- 审查者发现（**待处理**）：\n" + indentLines(f, "  ") + "\n")
	}
	if g.Exhausted() {
		sb.WriteString("- ⚠️ **预算已用尽**（" + strconv.Itoa(g.Round) + "/" +
			strconv.Itoa(g.MaxRounds) + "）。不要再调用目标验证，也不要宣布完成；" +
			"把未解决的问题连同证据一并告诉用户，说明需要人工介入。\n")
	} else {
		sb.WriteString("- 剩余可验证 " + strconv.Itoa(g.Remaining()) +
			" 轮。**先按发现修复，再重新验证**；不要在未做任何改动的情况下重复验证（原地打转）。\n")
	}
	return sb.String()
}

// indentLines 给多行文本每行加前缀。
func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// 账本接口与 ctx 槽位
// ---------------------------------------------------------------------------

// GoalLedger 是目标账本的操作接口。
//
// 实现是 agent.Session 上的四个方法（见 pkg/agent/goal.go）。声明成接口的
// 理由：pkg/tools 不能 import pkg/agent，而 goal_verify 工具必须能操作同一个账本。
type GoalLedger interface {
	// SetGoal 开启/更新目标，返回账本快照。
	SetGoal(goal string, maxRounds, step int) GoalState
	// Goal 返回账本快照。
	Goal() GoalState
	// RecordVerdict 记一次判定并推进轮次。
	RecordVerdict(v GoalVerdict, findings string, step int) GoalState
}

type goalLedgerKey struct{}

// WithGoalLedger 把当前会话的目标账本注入 context。
//
// 由服务端在每轮 run 的 ctx 上安装（与 WithApprover / WithSubagentSink 同一层），
// 装的必须是**本轮那个会话**的账本 —— 记到别处的话，注入的提示段永远为空。
func WithGoalLedger(ctx context.Context, l GoalLedger) context.Context {
	return context.WithValue(ctx, goalLedgerKey{}, l)
}

// GoalLedgerFrom 从 context 取出目标账本。
func GoalLedgerFrom(ctx context.Context) (GoalLedger, bool) {
	l, ok := ctx.Value(goalLedgerKey{}).(GoalLedger)
	return l, ok && l != nil
}

// GoalSessionLedger 返回一个把账本绑到具体会话 ID 的句柄。
//
// 账本必须挂在用户正在看的那一个会话上，所以启动层用它把 Session 与账本绑在一起；
// SessionID 供审批卡等处标识来源。
func GoalSessionLedger(sessionID string, l GoalLedger) GoalLedger { return l }

// ---------------------------------------------------------------------------
// 触发词
// ---------------------------------------------------------------------------

// GoalTriggerGoalMode 是目标模式的触发 token。
//
// 与 @plan 同一族约定：设置页与 @ 面板插入的是**插件 ID**，
// 而手打中文也能触发（用户常这么打，认两种更稳妥）。
const GoalTriggerGoalMode = "@goal_mode"

// GoalTriggerAliases 是 GoalTriggerGoalMode 之外接受的触发写法。
var GoalTriggerAliases = []string{"@目标模式", "@目标"}

// GoalTriggered 报告本轮输入是否触发了目标模式（大小写不敏感）。
func GoalTriggered(input string) bool {
	lower := strings.ToLower(input)
	if strings.Contains(lower, GoalTriggerGoalMode) {
		return true
	}
	for _, alias := range GoalTriggerAliases {
		if strings.Contains(lower, alias) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// VERDICT 解析
// ---------------------------------------------------------------------------

// ParseVerdict 从审查者的输出末尾解析判定结论。
//
// 策略：**从后往前找**最后一个能认出的判定 token，只在末行范围内匹配。
// 找不到返回 VerdictBlocked —— 调用方据此保持目标开启。
//
// 「找不到就 BLOCKED」是刻意的：判定读不出来绝不能当通过，
// 否则「审查员话没说完」会被记成「验证通过」，那正是本功能要消灭的假通过。
func ParseVerdict(output string) (GoalVerdict, string) {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	// 末行往往带空行/引用块前缀，只看最后 8 行足够且不会误捞到正文里的
	// 「例如如果是 PASS 就…」这类假设句。
	const tail = 8
	start := len(lines) - tail
	if start < 0 {
		start = 0
	}
	for i := len(lines) - 1; i >= start; i-- {
		if v, ok := scanVerdictLine(lines[i]); ok {
			return v, strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
	}
	// 退一步：整段文本里最后出现的那次也算（审查者把结论写在正文中间的情况）
	for i := len(lines) - 1; i >= 0; i-- {
		if v, ok := scanVerdictLine(lines[i]); ok {
			return v, ""
		}
	}
	return VerdictBlocked, ""
}

// scanVerdictLine 从单行文本里认出一个判定 token。
//
// ⚠️ 刻意宽松（不锚定行首）：模型常写成「**VERDICT: FAIL**」「判定：FAIL」
// 或把结论放进代码块。宁可从末行里捞出关键词，也不要因为格式不完美
// 把一次真实验证判成「读不出结论」。
func scanVerdictLine(line string) (GoalVerdict, bool) {
	s := strings.TrimSpace(line)
	// 剥掉 Markdown 强调与代码块围栏，免得 `**VERDICT: FAIL**` 匹配不到。
	s = strings.Trim(s, "*`>_# \t")
	upper := strings.ToUpper(s)
	for _, v := range []GoalVerdict{VerdictPass, VerdictFail, VerdictBlocked, VerdictSkip} {
		idx := strings.Index(upper, string(v))
		for idx >= 0 {
			if boundaryOK(upper, idx, len(v)) {
				return v, true
			}
			next := strings.Index(upper[idx+1:], string(v))
			if next < 0 {
				break
			}
			idx += next + 1
		}
	}
	return "", false
}

// IsVerdictLine 报告某行是否是判定行（供工具侧摘取发现清单时复用同一口径）。
func IsVerdictLine(line string) (GoalVerdict, bool) { return scanVerdictLine(line) }

// boundaryOK 判断 token 前后是否为非字母边界。
func boundaryOK(s string, start, length int) bool {
	var prev byte
	if start > 0 {
		prev = s[start-1]
	}
	var next byte
	if start+length < len(s) {
		next = s[start+length]
	}
	return !isASCIILetter(prev) && !isASCIILetter(next)
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ---------------------------------------------------------------------------
// 审查者结果
// ---------------------------------------------------------------------------

// GoalVerifyOutcome 是一轮审查的结果。
//
// 住在 pkg/tools 的理由同本文件开头：agent.GoalVerifier 要产出它、
// builtin.GoalVerifyTool 要消费它，而这两个包互相不能直接依赖。
type GoalVerifyOutcome struct {
	// Verdict 判定结论（读不出即为 BLOCKED，绝不会是 PASS）。
	Verdict GoalVerdict
	// Report 审查者的完整报告（给主智能体看的证据）。
	Report string
	// Steps 审查者实际用掉的步数。
	Steps int
	// Err 非空表示审查过程本身失败（模型不可用、被取消等）。
	Err error
}
