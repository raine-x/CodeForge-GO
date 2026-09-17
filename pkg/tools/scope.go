// scope.go 定义工作区围栏的执行器侧联动：ask 模式下越界必须人工审批；
// auto 模式全部自动通过；readonly 模式只放行只读工具。
package tools

import (
	"context"
	"encoding/json"
)

// ScopeChecker 由具备工作区围栏的工具实现：报告一次调用是否试图访问工作区之外。
//
// 执行器在策略判定为 Allow 之后询问本接口并按权限模式分流：ask 模式强制
// 升级为 Ask（人工审批），auto 模式自动放行，readonly 模式仅放行只读工具。
// 工具内部的原生围栏（如 FS.ResolveChecked）保持不变，作为最终防线。
type ScopeChecker interface {
	// OutsideScope 判断本次调用参数是否指向工作区之外。实现应保守：
	// 无法确认在区内时返回 true，宁可多弹一次审批。
	OutsideScope(args json.RawMessage) bool
}

// subagentScopeKey 是 context 中「子智能体路径围栏」的键类型。
type subagentScopeKey struct{}

// SubagentScope 描述子智能体允许访问的路径范围（相对于工作区根的相对路径）。
// Allowed 为空表示不限制（纯只读探索子任务常见）；Mode 用于诊断/审计。
type SubagentScope struct {
	Allowed []string
	Mode    string
}

// WithSubagentScope 将子智能体的路径围栏注入 context。
// 拥有围栏的子智能体，其文件工具只能访问 Allowed 内（或其子树）的路径。
func WithSubagentScope(ctx context.Context, sc SubagentScope) context.Context {
	return context.WithValue(ctx, subagentScopeKey{}, sc)
}

// SubagentScopeFrom 取出子智能体围栏。无围栏或围栏为空时返回 ok=false（不限制）。
func SubagentScopeFrom(ctx context.Context) (SubagentScope, bool) {
	v, ok := ctx.Value(subagentScopeKey{}).(SubagentScope)
	return v, ok && len(v.Allowed) > 0
}

// scopeApprovedKey 是 context 中「本次调用已获人工批准越界」的键类型。
type scopeApprovedKey struct{}

// subagentSinkKey 是 context 中「子智能体事件转发槽」的键类型。
type subagentSinkKey struct{}

// SubagentEvent 描述一条来自子智能体的实时进度事件（推送给前端）。
type SubagentEvent struct {
	ID      string `json:"id"`                // 子任务 ID
	Mode    string `json:"mode"`              // explore / implement
	Status  string `json:"status"`            // started / running / completed / failed
	Detail  string `json:"detail,omitempty"`  // 单行进度描述（工具活动等）
	Summary string `json:"summary,omitempty"` // 终态摘要（completed/failed 时）
}

// SubagentSink 接收子智能体事件（由 WS 层实现，转发给浏览器）。
type SubagentSink func(ev SubagentEvent)

// WithSubagentSink 将子智能体事件转发槽注入 context；无 sink 时事件被丢弃。
func WithSubagentSink(ctx context.Context, sink SubagentSink) context.Context {
	return context.WithValue(ctx, subagentSinkKey{}, sink)
}

// SubagentSinkFrom 取出事件转发槽；不存在时 ok=false。
func SubagentSinkFrom(ctx context.Context) (SubagentSink, bool) {
	v, ok := ctx.Value(subagentSinkKey{}).(SubagentSink)
	return v, ok && v != nil
}

// WithScopeApproved 标记本次工具调用已经人工审批放行；
// 工具内部围栏（FS.ResolveChecked）见到该标记后跳过越界拦截。
func WithScopeApproved(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeApprovedKey{}, true)
}

// ScopeApprovedFrom 返回本次调用是否已获人工批准越界。
func ScopeApprovedFrom(ctx context.Context) bool {
	v, ok := ctx.Value(scopeApprovedKey{}).(bool)
	return ok && v
}
