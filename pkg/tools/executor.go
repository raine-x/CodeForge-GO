package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codeforge/pkg/errs"
	"codeforge/pkg/security"
)

// Decision 描述一次工具调用的安全判定结果（供上层推送 HITL 事件）。
type Decision struct {
	Tool     string            `json:"tool"`
	Action   string            `json:"action"`
	Decision security.Decision `json:"decision"`
	Reason   string            `json:"reason,omitempty"`
}

// Executor 是工具安全执行器：统一执行超时控制、panic 恢复、输出截断、策略校验与审计落盘。
type Executor struct {
	registry  *Registry
	policy    *security.Policy
	audit     *security.AuditLogger
	approver  Approver
	timeout   time.Duration
	maxOutput int
	// approvalTimeout 是**人工审批自己的**等待上界，与 timeout（工具执行超时）
	// 是两件事，不能互相借用。
	approvalTimeout time.Duration
}

// DefaultApprovalTimeout 审批默认等待上限。
//
// 定这个值的依据：审批要等真人读完 diff 再决定，所以必须容得下正常的思考时间；
// 但又必须明显短于「连接一直挂着」这种无主场景，否则一个被遗忘的审批卡片会
// 永久占住执行器与 goroutine（此前正是这个状态：审批只受调用方 ctx 约束，
// 而生产链路的 ctx 由 WebSocket 连接生命周期决定，没有 deadline）。
const DefaultApprovalTimeout = 15 * time.Minute

// NewExecutor 构造安全执行器。
func NewExecutor(registry *Registry, policy *security.Policy, audit *security.AuditLogger, approver Approver, timeout time.Duration, maxOutput int) *Executor {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 32 * 1024
	}
	return &Executor{
		registry:        registry,
		policy:          policy,
		audit:           audit,
		approver:        approver,
		timeout:         timeout,
		maxOutput:       maxOutput,
		approvalTimeout: DefaultApprovalTimeout,
	}
}

// SetApprovalTimeout 覆盖审批等待上限。<=0 时回落到 DefaultApprovalTimeout。
func (e *Executor) SetApprovalTimeout(d time.Duration) {
	if d <= 0 {
		d = DefaultApprovalTimeout
	}
	e.approvalTimeout = d
}

// Registry 返回底层工具注册中心。
func (e *Executor) Registry() *Registry { return e.registry }

// Timeout 返回工具执行的默认超时（单条工具可用 Schema 覆盖）。
func (e *Executor) Timeout() time.Duration { return e.timeout }

// Approver 返回当前的人工审批通道，可能为 nil（表示无法弹审批）。
func (e *Executor) Approver() Approver { return e.approver }

// ApprovalTimeout 返回审批等待上限。
func (e *Executor) ApprovalTimeout() time.Duration { return e.approvalTimeout }

// MaxOutput 返回输出截断阈值。
func (e *Executor) MaxOutput() int { return e.maxOutput }

// SetApprover 在服务启动后注入审批实现。
func (e *Executor) SetApprover(a Approver) { e.approver = a }

// Policy 返回当前安全策略引擎（供 /api/perm 查询与热切换）。
func (e *Executor) Policy() *security.Policy { return e.policy }

// Audit 返回底层审计日志器（可能为 nil）。
//
// 存在的理由：派生执行器时要能**继承真审计**。子智能体那套传的是 nil
// （pkg/agent/subagent.go），因为子任务的动作都由主智能体在别处留痕；
// 但目标模式的审查者会**自主执行命令**（起服务、跑测试），那些命令必须
// 进 audit.jsonl —— 否则「智能体自己跑了什么」无从追溯。
func (e *Executor) Audit() *security.AuditLogger { return e.audit }

// Evaluate 仅做安全判定，不执行工具。
func (e *Executor) Evaluate(name string, args json.RawMessage) Decision {
	action := extractAction(name, args)
	tool, hasTool := e.registry.Get(name)
	var readOnly bool
	if hasTool {
		readOnly = isReadOnlyCall(tool, args)
	}
	d, reason := e.policy.Evaluate(name, action, string(args), readOnly)
	if hasTool {
		d, reason, _ = e.escalate(tool, name, args, d, reason)
	}
	return Decision{Tool: name, Action: action, Decision: d, Reason: reason}
}

// decide 是 Execute 用的完整判定：策略引擎 + 按模式的越界处理，二者结论一致。
// 第三个返回值是「本次调用是否指向工作区之外」，Execute 用它决定要不要给这一次
// 执行注入围栏豁免（见 Execute 里 ScopeApproved 的两处注入）。
func (e *Executor) decide(tool Tool, name, action string, args json.RawMessage) (security.Decision, string, bool) {
	d, reason := e.policy.Evaluate(name, action, string(args), isReadOnlyCall(tool, args))
	return e.escalate(tool, name, args, d, reason)
}

// scopeNote 是越界时对用户披露的说明。
//
// ⚠️ 它必须出现在**每一次**越界审批的 reason 里。早先 escalate 在判定非 Allow 时
// 直接返回，于是默认配置（permission_mode=ask + write_file/edit_file/delete_file/
// run_command/web_* 都在 ask 规则里）下这些工具**先**拿到 Ask、越界检查压根没跑，
// 审批弹窗只说「命中规则：write_file」—— 用户完全看不出这一步要跳出工作区，
// 而批准后 ScopeApproved 又会让工具内部的 checkScope 一并跳过。
const scopeNote = "目标越出工作区范围，需人工审批（任何权限模式下越界访问都必须手动批准）"

// escalate 处理「目标越出工作区」的情况，按权限模式分流：
//   - auto（自主）：全部自动通过，不弹审批 —— 自主模式不请求人工确认；
//   - readonly（只读）：只读工具（含声明 IsReadOnly 的探索/搜索类工具）
//     区内外都放行；非只读工具维持拒绝，不升级为审批；
//   - 其余模式（ask 等）：越界强制升级为 Ask。
//
// 第三个返回值是「是否越界」，与「是否放行」**刻意分开**：前者决定要不要给这一次
// 执行注入围栏豁免，后者只决定策略结论。
//
// Deny 维持原判（黑名单优先级最高，审批不能解锁）。
//
// ⚠️ 越界检查**必须对任何 incoming decision 都跑**。早先写成
// `if d != Allow { return }`，而默认配置下危险的那批工具本来就是 Ask ——
// 于是 ScopeChecker 从未被问，越界披露与围栏收紧双双失效。
func (e *Executor) escalate(tool Tool, name string, args json.RawMessage, d security.Decision, reason string) (security.Decision, string, bool) {
	sc, ok := tool.(ScopeChecker)
	outOfScope := ok && sc.OutsideScope(args)
	if !outOfScope {
		return d, reason, false
	}
	if d == security.Deny {
		return d, reason, true
	}
	switch e.policy.Mode() {
	case security.ModeAuto:
		// 自主模式不弹审批：放行本次调用，Execute 会一并注入围栏豁免。
		return security.Allow, "自主模式自动放行（越界访问不再请求审批）", true
	case security.ModeReadOnly:
		// 只认工具**自己**的声明，不看名字。
		//
		// 早先这里查 security.IsReadOnlyTool(name) —— 一张按名字硬编码的表。
		// 两个方向都出过事：声明了只读但不在表里的 find_files / web_fetch
		// 被误拒；只在表里却没有任何声明的 git_status / git_log 被凭名字放行，
		// 而工具名是插件/MCP 可控的。
		//
		// 未声明 = 不知道 = 拒绝。这是 fail-closed，也是「漏实现」不再是
		// 静默降级的关键。
		if meta, ok := MetadataOf(tool); ok && meta.SideEffect == SideEffectNone {
			return security.Allow, "只读模式放行只读工具：" + name, true
		}
		return security.Deny, "只读模式禁止非只读工具：" + name, true
	default:
		if d == security.Allow {
			return security.Ask, scopeNote, true
		}
		// 原本就是 Ask（多半命中了 ask 规则）：**并入**越界说明而不是丢掉原原因。
		// 两段都在弹窗里，用户既知道「为什么需要审批」，也知道「它要跳出工作区」。
		return security.Ask, reason + "；" + scopeNote, true
	}
}

// Execute 按安全策略执行一次工具调用。
func (e *Executor) Execute(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	start := time.Now()
	tool, ok := e.registry.Get(name)
	if !ok {
		return Err("未知工具: %s", name), nil
	}

	action := extractAction(name, args)
	decision, reason, outOfScope := e.decide(tool, name, action, args)
	entry := security.AuditEntry{Tool: name, Action: action, Decision: string(decision)}

	switch decision {
	case security.Deny:
		entry.Success = false
		entry.Error = "denied"
		entry.DurationMs = time.Since(start).Milliseconds()
		_ = e.audit.Log(entry)
		return Err("操作被安全策略拒绝：%s", reason), nil

	case security.Ask:
		approver := e.approver
		if a, ok := ApproverFrom(ctx); ok && a != nil {
			approver = a
		}
		if approver == nil {
			entry.Success = false
			entry.Error = "no-approver"
			entry.DurationMs = time.Since(start).Milliseconds()
			_ = e.audit.Log(entry)
			return Err("该操作需要人工审批，但当前无可用审批通道"), nil
		}
		req := ApprovalRequest{Tool: name, Action: action, Reason: reason, Detail: string(args)}
		// 子智能体发起的审批带上子任务身份：并行委派时界面上会同时挂多张卡片，
		// 没有这个字段它们长得一模一样，用户批错那张照样授权一次真实的写入。
		// 用 SubagentIdentityFrom 而不是 SubagentScopeFrom：后者的 ok 依赖
		// len(Allowed)>0（围栏语义），而 explore 子任务不必声明 paths。
		if id, mode, ok := SubagentIdentityFrom(ctx); ok {
			req.SubagentID = id
			req.SubagentMode = mode
		}
		if dp, ok := tool.(DiffProvider); ok {
			if diff, err := dp.PreviewDiff(args); err == nil {
				req.Diff = diff
			}
		}
		// 审批有**自己**的上界，不借用 timeout。
		// 此前这里直接传 Execute 的入参 ctx，而生产链路的 ctx 由 WebSocket
		// 连接生命周期决定、没有 deadline —— 审批卡片被遗忘就会永久挂住
		// 执行器与 goroutine。approvalTimeout 补上这个缺口；
		// 若调用方 ctx 更短（如用户关页面），WithTimeout 仍以先到的为准。
		approvalTimeout := e.approvalTimeout
		if approvalTimeout <= 0 {
			approvalTimeout = DefaultApprovalTimeout
		}
		actx, cancelApproval := context.WithTimeout(ctx, approvalTimeout)
		approved, err := approver.RequestApproval(actx, req)
		timedOut := errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
		cancelApproval()
		if err != nil {
			entry.Success = false
			// 审计留原始错误（要可追溯），给用户/模型的说明则翻译成人话
			entry.Error = "approval-error: " + err.Error()
			entry.DurationMs = time.Since(start).Milliseconds()
			_ = e.audit.Log(entry)
			if timedOut {
				// 超时**不是拒绝**。两者在界面上必须能区分：
				// 「用户说不」和「没人管」对下一次决策的含义完全不同。
				return Err("请求操作审批超时（超过 %s 无人处理，该操作未执行）", approvalTimeout), nil
			}
			return Err("%s", errs.FriendlyOr("请求操作审批", err)), nil
		}
		entry.Approved = &approved
		if !approved {
			entry.Success = false
			entry.Error = "rejected"
			entry.DurationMs = time.Since(start).Milliseconds()
			_ = e.audit.Log(entry)
			return Err("用户拒绝了该操作"), nil
		}
		// ⚠️ 只有「这次审批确实放行了一个越界目标」才注入围栏豁免。
		// 早先这里对**任何**被批准的 Ask 都注入，于是「因命中 ask 规则而批准的一次
		// 区内操作」会顺带把工具内部的 checkScope 关掉（FS.ResolveChecked 见
		// ScopeApproved 即跳过）—— 工作区围栏等于形同虚设。
		if outOfScope {
			ctx = WithScopeApproved(ctx)
		}
	}

	// auto / readonly 模式自动放行越界：同样只对这一次调用豁免围栏。
	if outOfScope && decision == security.Allow {
		ctx = WithScopeApproved(ctx)
	}

	res, err := e.run(ctx, tool, args)
	if err != nil {
		entry.Success = false
		entry.Error = err.Error() // 审计留原始错误
		entry.DurationMs = time.Since(start).Milliseconds()
		_ = e.audit.Log(entry)
		if res != nil {
			return res, nil
		}
		// 给模型/用户的说明带上工具名与成因 —— 光说「工具执行失败」等于没说
		return Err("%s", errs.FriendlyOr("执行工具 "+name, err)), nil
	}
	res = e.truncate(res)

	entry.Success = res == nil || res.Success
	if res != nil && !res.Success {
		entry.Error = res.Error
	}
	entry.DurationMs = time.Since(start).Milliseconds()
	_ = e.audit.Log(entry)
	return res, nil
}

type runOutcome struct {
	res *ToolResult
	err error
}

// timeoutFor 决定本次执行的兜底超时。
//
// 默认对所有工具都是 e.timeout（120s）。工具若实现 TimeoutPolicy，可以按入参
// 申请更长的阈值 —— 目前只有 goal_verify 需要：它一次调用内部要跑一整轮审查者
// 子智能体（多次 LLM 往返，还可能起服务等它起来），120s 必然砍断。
//
// 三条纪律：
//   - 只允许**放宽**，不允许收紧。收紧留给工具自己（轮数/步数），
//     统一在这里收紧会让「默认 120s 防失控」这条兜底被单个工具悄悄拆掉。
//   - 拿不到正数就回落默认值，不返回 0 —— context.WithTimeout(0) 是立即超时，
//     一次误判就会让工具秒失败，比超时更难查。
//   - cctx 仍然派生自 ctx，用户点停止照样立刻中断（见 TimeoutPolicy 的注释）。
func (e *Executor) timeoutFor(tool Tool, args json.RawMessage) time.Duration {
	tp, ok := tool.(TimeoutPolicy)
	if !ok {
		return e.timeout
	}
	d := tp.ToolTimeout(args)
	if d <= 0 {
		return e.timeout
	}
	if d < e.timeout {
		return e.timeout
	}
	return d
}

// run 在独立 goroutine 中执行工具，实现超时控制与 panic 恢复。
func (e *Executor) run(ctx context.Context, tool Tool, args json.RawMessage) (*ToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeoutFor(tool, args))
	defer cancel()

	ch := make(chan runOutcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- runOutcome{res: Err("工具执行 panic: %v", r)}
			}
		}()
		res, err := tool.Execute(cctx, args)
		ch <- runOutcome{res: res, err: err}
	}()

	select {
	case out := <-ch:
		return out.res, out.err
	case <-cctx.Done():
		return Err("工具执行超时或被取消"), cctx.Err()
	}
}

// truncate 对超大输出做限幅，避免撑爆上下文。
func (e *Executor) truncate(res *ToolResult) *ToolResult {
	if res == nil {
		return res
	}
	if s, ok := res.Data.(string); ok && len(s) > e.maxOutput {
		res.Data = truncateString(s, e.maxOutput) + fmt.Sprintf("\n... [输出已截断，原始长度 %d 字节]", len(s))
		return res
	}
	data, err := json.Marshal(res.Data)
	if err == nil && len(data) > e.maxOutput {
		res.Data = truncateString(string(data), e.maxOutput) + fmt.Sprintf("\n... [结果已截断，原始长度 %d 字节]", len(data))
	}
	return res
}

// truncateString 在 rune 边界安全截断字符串。
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// extractAction 从参数中提取语义动作（命令 / 路径等），用于安全判定。
func extractAction(name string, args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return name
	}
	for _, k := range []string{"command", "path", "file_path", "pattern", "query"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return name
}
