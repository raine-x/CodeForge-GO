package tools

import "time"

// Role 是执行器的**角色**。
//
// 存在的理由：`NewExecutor` 的六个参数里有两个是「必须照抄父执行器」的
// （audit、policy），第三个（approver）按角色决定。手工传参时抄错一个就是
// 静默降级 —— 真实历史是子智能体把 audit 传成了 nil，于是它的每一次写入和
// 每一次命令执行都不进审计且**不报错**。
//
// 改成角色工厂后，审计不再出现在签名里：调用方无从传错。
type Role string

const (
	// RoleMain 是主对话的执行器。
	RoleMain Role = "main"
	// RoleSubagent 是子智能体（explore / implement）的执行器。
	RoleSubagent Role = "subagent"
	// RoleGoalReviewer 是目标模式审查者的执行器。
	RoleGoalReviewer Role = "goal_reviewer"
)

// roleSpec 是一个角色的固定参数。
type roleSpec struct {
	timeout   time.Duration
	maxOutput int
	// inheritApprover 决定是否继承父执行器的审批通道。
	inheritApprover bool
}

// roleSpecs 是各角色的参数表。
//
// 子智能体与审查者**不**继承审批通道，这是刻意的：两者都跑在后台 goroutine 上，
// 给它们审批通道等于让界面上出现没有会话归属的孤儿卡片。命中 ask 规则时
// 应当直接拒绝（`no-approver`），而不是挂死等一个永远不会来的决策。
//
// 审查者的工具超时**比默认宽**（5 分钟）：审查动作天然是长跑 ——
// 跑测试、扫全库、做交叉验证，120s 会让它在半途硬停，然后交一份
// 「还没验完」的结论把活儿又干一遍（goal_runner_test 有测试钉这条）。
var roleSpecs = map[Role]roleSpec{
	RoleMain:         {timeout: 120 * time.Second, maxOutput: 32 * 1024, inheritApprover: true},
	RoleSubagent:     {timeout: 120 * time.Second, maxOutput: 32 * 1024, inheritApprover: false},
	RoleGoalReviewer: {timeout: 5 * time.Minute, maxOutput: 32 * 1024, inheritApprover: false},
}

// RoleTimeout 返回某角色的工具执行超时。
func RoleTimeout(role Role) time.Duration {
	if spec, ok := roleSpecs[role]; ok {
		return spec.timeout
	}
	return roleSpecs[RoleMain].timeout
}

// NewExecutorFor 按角色派生一个执行器。
//
// registry 是该角色能看到的工具表（已按白名单裁剪），parent 提供
// **不可省略**的策略与审计通道。
//
// 三个要点：
//
//  1. audit 与 policy 一律从 parent 取，不给调用方传的机会 —— 这是本函数存在的全部意义。
//  2. timeout / maxOutput 按角色固定，不从 parent 继承。父的超时可能是给
//     交互式主循环调的预算，泄给子角色没有道理。
//  3. approver 按角色决定，见 roleSpecs 的说明。
func NewExecutorFor(parent *Executor, role Role, registry *Registry) *Executor {
	spec, ok := roleSpecs[role]
	if !ok {
		// 未知角色退回主循环参数：宁可宽一点，也不要静默给 0 超时。
		spec = roleSpecs[RoleMain]
	}

	e := &Executor{
		registry:        registry,
		policy:          parent.Policy(),
		audit:           parent.Audit(),
		timeout:         spec.timeout,
		maxOutput:       spec.maxOutput,
		approvalTimeout: parent.ApprovalTimeout(),
	}
	if spec.inheritApprover {
		e.approver = parent.Approver()
	}
	return e
}
