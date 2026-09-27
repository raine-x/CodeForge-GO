// goal.go 把目标账本挂到 Session 上。
//
// 纯数据类型（判定结论、账本快照、VERDICT 解析、GoalLedger 接口与 ctx 槽位）
// 住在 pkg/tools/goal.go —— 因为 goal_verify 工具在 pkg/tools/builtin 里，
// 它必须能操作同一个账本，而 pkg/agent 的同包测试已经 import 了 builtin
// （diffstats_test.go），反向引用会成环。这里用类型别名把数据接回来，
// 既有写法（agent.GoalState / agent.VerdictPass / agent.ParseVerdict）不变。
package agent

import (
	"strings"
	"time"

	"codeforge/pkg/tools"
)

// 类型别名：让 agent 侧继续按 agent.GoalState 写，底下是 tools 里的同一份定义。
type (
	// GoalVerdict 是审查者的判定结论（PASS/FAIL/BLOCKED/SKIP）。
	GoalVerdict = tools.GoalVerdict
	// GoalState 是目标账本快照。
	GoalState = tools.GoalState
	// GoalLedger 是账本操作接口。
	GoalLedger = tools.GoalLedger
)

const (
	VerdictPass         = tools.VerdictPass
	VerdictFail         = tools.VerdictFail
	VerdictBlocked      = tools.VerdictBlocked
	VerdictSkip         = tools.VerdictSkip
	GoalTriggerGoalMode = tools.GoalTriggerGoalMode
)

// ParseVerdict 从审查者输出末尾解析判定（读不出即 BLOCKED，绝不会是 PASS）。
func ParseVerdict(output string) (GoalVerdict, string) { return tools.ParseVerdict(output) }

// GoalTriggered 报告本轮输入是否触发了目标模式。
func GoalTriggered(input string) bool { return tools.GoalTriggered(input) }

// SetGoal 开启或更新目标。
//
// 两种情形必须分开处理，否则会出两种错：
//   - 账本**还开着**（同一目标继续）：只更新目标文本，轮次**不重置**。
//     模型改写措辞不等于换目标，否则可以靠反复改写绕过预算上限。
//   - 账本**已关闭**（上一目标已通过）或从没有过：**从零开始**。
//     复用旧结构体会把上一目标的轮次带过来，于是新目标一开局就显示已用掉 1 轮。
func (s *Session) SetGoal(goal string, maxRounds, step int) GoalState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal == nil || s.goal.Goal == "" {
		s.goal = &GoalState{MaxRounds: maxRounds}
	}
	s.goal.Goal = strings.TrimSpace(goal)
	if maxRounds > 0 {
		s.goal.MaxRounds = maxRounds
	}
	s.goal.LastStep = step
	return s.goal.Clone()
}

// Goal 返回当前账本快照（没有开启的目标时返回零值，其 Open() 为 false）。
func (s *Session) Goal() GoalState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.goal == nil {
		return GoalState{}
	}
	return s.goal.Clone()
}

// RecordVerdict 记一次验证结果并推进轮次。
//
// 轮次在**每次验证后**都 +1，包括 Pass 那次 —— 它同样花掉了一轮预算。
//
// Pass 时关闭账本。⚠️ 关闭必须在取快照**之前**：否则返回的快照里 Goal 仍非空，
// Open() 为 true，调用方（goal_verify）会以为目标还开着、下一轮接着验。
// 因此返回值在 Pass 时 Goal 为空 —— 要显示「目标 X 已通过」请用自己传进来的目标原文。
func (s *Session) RecordVerdict(v GoalVerdict, findings string, step int) GoalState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal == nil {
		// 没有账本却来记判定：不该发生（goal_verify 总会先 SetGoal）。
		// 这里建一个空目标并原样记账，避免静默丢证据。
		s.goal = &GoalState{MaxRounds: 1}
	}
	s.goal.Round++
	s.goal.LastVerdict = v
	s.goal.LastFindings = strings.TrimSpace(findings)
	s.goal.LastStep = step
	s.goal.VerifiedAt = time.Now()
	if v.Passes() {
		s.goal.Goal = "" // 账本关闭
	}
	return s.goal.Clone()
}

// ClearGoal 强制关闭账本（清空目标与发现，轮次保留）。
//
// 只给「用户显式放弃目标」用。预算用尽**不能**走这里：那属于「停下来问人」，
// 而问人之前得让人看到问题。
func (s *Session) ClearGoal() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.goal == nil {
		return
	}
	s.goal.Goal = ""
	s.goal.LastFindings = ""
	s.goal.LastVerdict = ""
}
