package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
}

// NewExecutor 构造安全执行器。
func NewExecutor(registry *Registry, policy *security.Policy, audit *security.AuditLogger, approver Approver, timeout time.Duration, maxOutput int) *Executor {
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 32 * 1024
	}
	return &Executor{
		registry:  registry,
		policy:    policy,
		audit:     audit,
		approver:  approver,
		timeout:   timeout,
		maxOutput: maxOutput,
	}
}

// Registry 返回底层工具注册中心。
func (e *Executor) Registry() *Registry { return e.registry }

// SetApprover 在服务启动后注入审批实现。
func (e *Executor) SetApprover(a Approver) { e.approver = a }

// Policy 返回当前安全策略引擎（供 /api/perm 查询与热切换）。
func (e *Executor) Policy() *security.Policy { return e.policy }

// Evaluate 仅做安全判定，不执行工具。
func (e *Executor) Evaluate(name string, args json.RawMessage) Decision {
	action := extractAction(name, args)
	d, reason := e.policy.Evaluate(name, action, string(args))
	if tool, ok := e.registry.Get(name); ok {
		d, reason = escalateOutsideScope(tool, args, d, reason)
	}
	return Decision{Tool: name, Action: action, Decision: d, Reason: reason}
}

// decide 是 Execute 用的完整判定：策略引擎 + 越界强制审批，二者结论一致。
func (e *Executor) decide(tool Tool, name, action string, args json.RawMessage) (security.Decision, string) {
	d, reason := e.policy.Evaluate(name, action, string(args))
	return escalateOutsideScope(tool, args, d, reason)
}

// escalateOutsideScope 把「越界但被静默放行」的判定升级为 Ask：
// 策略给出 Allow 时，若工具报告本次调用越出工作区，则无论处于什么权限模式
// （readonly / ask / auto）都强制人工审批；Deny 与既有 Ask 维持原判。
func escalateOutsideScope(tool Tool, args json.RawMessage, d security.Decision, reason string) (security.Decision, string) {
	if d != security.Allow {
		return d, reason
	}
	if sc, ok := tool.(ScopeChecker); ok && sc.OutsideScope(args) {
		return security.Ask, "目标越出工作区范围，需人工审批（任何权限模式下越界访问都必须手动批准）"
	}
	return d, reason
}

// Execute 按安全策略执行一次工具调用。
func (e *Executor) Execute(ctx context.Context, name string, args json.RawMessage) (*ToolResult, error) {
	start := time.Now()
	tool, ok := e.registry.Get(name)
	if !ok {
		return Err("未知工具: %s", name), nil
	}

	action := extractAction(name, args)
	decision, reason := e.decide(tool, name, action, args)
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
		if dp, ok := tool.(DiffProvider); ok {
			if diff, err := dp.PreviewDiff(args); err == nil {
				req.Diff = diff
			}
		}
		approved, err := approver.RequestApproval(ctx, req)
		if err != nil {
			entry.Success = false
			entry.Error = "approval-error: " + err.Error()
			entry.DurationMs = time.Since(start).Milliseconds()
			_ = e.audit.Log(entry)
			return Err("审批通道异常：%s", err.Error()), nil
		}
		entry.Approved = &approved
		if !approved {
			entry.Success = false
			entry.Error = "rejected"
			entry.DurationMs = time.Since(start).Milliseconds()
			_ = e.audit.Log(entry)
			return Err("用户拒绝了该操作"), nil
		}
		// 人工已批准本次调用：注入放行标记，工具内部围栏（FS.ResolveChecked）
		// 对这一次执行跳过越界拦截；未批准的越界调用在此前已被拒绝。
		ctx = WithScopeApproved(ctx)
	}

	res, err := e.run(ctx, tool, args)
	if err != nil {
		entry.Success = false
		entry.Error = err.Error()
		entry.DurationMs = time.Since(start).Milliseconds()
		_ = e.audit.Log(entry)
		if res != nil {
			return res, nil
		}
		return Err("工具执行失败：%s", err.Error()), nil
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

// run 在独立 goroutine 中执行工具，实现超时控制与 panic 恢复。
func (e *Executor) run(ctx context.Context, tool Tool, args json.RawMessage) (*ToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, e.timeout)
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
