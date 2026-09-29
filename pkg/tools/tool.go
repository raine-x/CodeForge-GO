// Package tools 定义 Agent 可调用工具的统一抽象、注册中心与安全执行器。
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Tool 代表 Agent 可调用的统一工具接口。
type Tool interface {
	// Name 返回工具唯一名称。
	Name() string
	// Description 返回供 LLM 理解用途的描述。
	Description() string
	// InputSchema 返回 JSON Schema，供 LLM 进行 Tool Choice 拼接。
	InputSchema() json.RawMessage
	// Execute 执行工具调用。
	Execute(ctx context.Context, args json.RawMessage) (*ToolResult, error)
}

// ReadOnlyTool 可由工具实现，用于显式声明其是否只读。
//
// ⚠️ 已废弃：请改用 Metadata().SideEffect。保留它只是为了不破坏外部实现，
// 判定侧不再读它（IsReadOnly 与 SideEffect 可能互相矛盾，工具会同时实现两者）。
type ReadOnlyTool interface {
	IsReadOnly() bool
}

// SideEffect 声明工具对世界的副作用等级。
//
// 之所以要这个：早先「是否只读」有两处真相来源 —— 工具实现的 IsReadOnly()
// 与 security 包里一张按名字硬编码的表。两边不一致，而且**两个方向都出错**：
// 声明了只读但不在表里的工具被误拒；只在表里却没声明的工具被凭名字放行，
// 而工具名是插件/MCP 可控的，等于给插件开了提权口子。
//
// 声明放在工具上、只有一处、且漏声明会 fail-closed，这三个性质一起把
// 「静默降级」这个反模式关掉了（见 AGENTS.md 硬规则 3）。
type SideEffect int

const (
	// SideEffectNone 纯读：读文件、列目录、搜索、抓网页。不改变任何状态，
	// 可与任何其他调用并发，也可被只读模式自动放行。
	SideEffectNone SideEffect = iota
	// SideEffectWrite 写工作区内的内容。可逆（快照可撤销）。
	SideEffectWrite
	// SideEffectExternal 影响工作区之外：执行命令、写工作区外文件、发网络请求。
	// 不可由工具自己保证可逆，故与 Write 分开。
	SideEffectExternal
	// SideEffectDestructive 破坏性：删除、覆盖既有内容且无法自动撤销。
	// 与 Write 分开是因为「可撤销」这件事本身需要用户单独确认。
	SideEffectDestructive
)

// Valid 报告取值是否在枚举范围内。
//
// 判定侧遇到非法取值一律按最严处理，因此这个方法主要用于**测试**：
// 声明里写错等级会立刻被 CI 抓到，而不是悄悄退化。
func (s SideEffect) Valid() bool { return s >= SideEffectNone && s <= SideEffectDestructive }

// String 用于审计与调试输出。
func (s SideEffect) String() string {
	switch s {
	case SideEffectNone:
		return "none"
	case SideEffectWrite:
		return "write"
	case SideEffectExternal:
		return "external"
	case SideEffectDestructive:
		return "destructive"
	default:
		return "invalid"
	}
}

// Metadata 是工具的静态声明面。
//
// 参照 ZCode 的 ToolMetadata（core/src/tool/types.ts:68-97），
// 但只保留本项目当下真正有消费者的字段。刻意**不**加：
//   - ConcurrentSafe：批量并发调度属 2.6（ReAct 循环状态化），已推迟；
//     现在加就是个没人读的字段。
//   - riskLevel / timeoutMs：已有 TimeoutPolicy 接口在承担同样的职责，
//     重复声明只会造成第二个真相来源。
type Metadata struct {
	// SideEffect 副作用等级，必填。
	SideEffect SideEffect
}

// Metadated 由需要声明副作用等级的工具实现。
//
// 未实现它的工具在**只读模式下会被拒绝**（fail-closed）：
// 不知道副作用等级时，唯一安全的假设是「它有副作用」。
type Metadated interface {
	Metadata() Metadata
}

// MetadataOf 取工具的声明。第二个返回值为 false 表示该工具没有声明。
//
// 判定侧必须用这个函数而不是直接类型断言，这样「未声明」与「声明为 none」
// 才不会被混为一谈 —— 前者要拒绝，后者要放行。
func MetadataOf(t Tool) (Metadata, bool) {
	m, ok := t.(Metadated)
	if !ok {
		return Metadata{}, false
	}
	meta := m.Metadata()
	// 非法取值当作「没声明」，走 fail-closed。
	if !meta.SideEffect.Valid() {
		return Metadata{}, false
	}
	return meta, true
}

// SubagentTask 描述一个可并行委派的独立子任务。
// Mode=explore 时只能使用只读工具；Mode=implement 时允许在声明的路径范围内修改。
type SubagentTask struct {
	ID     string   `json:"id"`
	Prompt string   `json:"prompt"`
	Mode   string   `json:"mode"`
	Paths  []string `json:"paths,omitempty"`
}

// SubagentResult 是子智能体返回给主智能体的摘要。
type SubagentResult struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
}

// SubagentRunner 是内置多智能体工具依赖的调度接口。
type SubagentRunner interface {
	RunSubagents(ctx context.Context, tasks []SubagentTask) ([]SubagentResult, error)
}

// ToolResult 定义工具调用的统一标准化输出。
type ToolResult struct {
	Success  bool           `json:"success"`
	Data     interface{}    `json:"data,omitempty"`
	Error    string         `json:"error,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Ok 构造一个成功结果。
func Ok(data interface{}) *ToolResult {
	return &ToolResult{Success: true, Data: data}
}

// OkMeta 构造一个带元数据的成功结果。
func OkMeta(data interface{}, meta map[string]any) *ToolResult {
	return &ToolResult{Success: true, Data: data, Metadata: meta}
}

// Err 构造一个失败结果。
func Err(format string, a ...any) *ToolResult {
	return &ToolResult{Success: false, Error: fmt.Sprintf(format, a...)}
}

// ApprovalRequest 描述一次待审批的危险操作。
type ApprovalRequest struct {
	Tool   string `json:"tool"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	Diff   string `json:"diff,omitempty"`
	// SubagentID 是发起这次审批的子任务 id（omitempty = 主智能体发起的）。
	//
	// 一次委派最多并行 5 个子智能体（config.SubagentConcurrencyCap），它们的审批
	// 会同时挂在界面上。没有这个字段时那些卡片长得**完全一样**（中文短语相同、
	// session_id 也相同），用户无从分辨，批错那张照样授权一次真实的、不同的写入。
	SubagentID string `json:"subagent_id,omitempty"`
	// SubagentMode 同样只给子智能体用：explore / implement。
	SubagentMode string `json:"subagent_mode,omitempty"`
}

// Approver 由上层（Web 服务）实现，用于 HITL 人工审批。
type Approver interface {
	// RequestApproval 挂起当前流程等待用户决策；返回 true 表示批准。
	RequestApproval(ctx context.Context, req ApprovalRequest) (bool, error)
}

// approverKey 是 context 中审批器的键类型。
type approverKey struct{}

// WithApprover 将审批器注入 context（按会话隔离，优先于执行器默认审批器）。
func WithApprover(ctx context.Context, a Approver) context.Context {
	return context.WithValue(ctx, approverKey{}, a)
}

// ApproverFrom 从 context 取出审批器。
func ApproverFrom(ctx context.Context) (Approver, bool) {
	a, ok := ctx.Value(approverKey{}).(Approver)
	return a, ok
}

// DiffProvider 可由工具实现，返回本次操作的 Unified Diff（供 HITL 展示）。
type DiffProvider interface {
	PreviewDiff(args json.RawMessage) (string, error)
}

// ReadGate 由「要求先读后写」的写工具实现：作废某会话已登记的文件阅读记录。
//
// 上下文压缩把文件原文挤出送模视图之后，模型手里只剩摘要 —— 此刻它对那份文件
// 「读过」的事实已经不成立，登记必须随之作废，否则压缩后凭记忆整体重写照样放行。
type ReadGate interface {
	ForgetReads(sessionID string)
}

// TimeoutPolicy 由「单次调用耗时远超常规」的工具实现，申请自己的执行超时。
//
// 为什么需要它：Executor.run 给**所有**工具套同一个 e.timeout（默认 120s，见
// NewExecutor）。那是防工具失控的兜底，但对「一次调用内部要跑多轮 LLM、可能还要
// 起服务等它起来」的长任务必然不够 —— 120s 一到，工具连同它发起的嵌套 LLM 请求
// 一起被砍掉，调用方只看到「工具执行超时或被取消」，什么也没发生。
//
// ⚠️ 放宽的是**单个工具的兜底阈值**，不是取消机制：cctx 仍然派生自调用方的 ctx，
// 所以用户点停止 / 会话被取消时照样立刻中断。不要在这里实现业务超时 ——
// 业务上限属于工具自己的事（轮数、步数）。
type TimeoutPolicy interface {
	// ToolTimeout 返回本次调用的执行超时；返回 0 或负数表示沿用执行器默认值。
	ToolTimeout(args json.RawMessage) time.Duration
}

// ---------------------------------------------------------------------------
// JSON Schema 构造辅助
// ---------------------------------------------------------------------------

// SchemaBuilder 用于以链式方式构造 JSON Schema 对象。
type SchemaBuilder struct {
	props    map[string]map[string]any
	required []string
}

// NewSchema 创建一个空的 SchemaBuilder。
func NewSchema() *SchemaBuilder {
	return &SchemaBuilder{props: map[string]map[string]any{}}
}

// Prop 添加一个属性定义。
func (s *SchemaBuilder) Prop(name, typ, desc string, required bool) *SchemaBuilder {
	s.props[name] = map[string]any{"type": typ, "description": desc}
	if required {
		s.required = append(s.required, name)
	}
	return s
}

// Str 添加字符串属性。
func (s *SchemaBuilder) Str(name, desc string, required bool) *SchemaBuilder {
	return s.Prop(name, "string", desc, required)
}

// Int 添加整数属性。
func (s *SchemaBuilder) Int(name, desc string, required bool) *SchemaBuilder {
	return s.Prop(name, "integer", desc, required)
}

// Bool 添加布尔属性。
func (s *SchemaBuilder) Bool(name, desc string, required bool) *SchemaBuilder {
	return s.Prop(name, "boolean", desc, required)
}

// Enum 添加枚举字符串属性。
func (s *SchemaBuilder) Enum(name, desc string, values []string, required bool) *SchemaBuilder {
	s.props[name] = map[string]any{"type": "string", "description": desc, "enum": values}
	if required {
		s.required = append(s.required, name)
	}
	return s
}

// ArrayOfStr 添加字符串数组属性。
func (s *SchemaBuilder) ArrayOfStr(name, desc string, required bool) *SchemaBuilder {
	s.props[name] = map[string]any{
		"type":        "array",
		"description": desc,
		"items":       map[string]any{"type": "string"},
	}
	if required {
		s.required = append(s.required, name)
	}
	return s
}

// Build 生成 JSON Schema 字节。
func (s *SchemaBuilder) Build() json.RawMessage {
	obj := map[string]any{
		"type":       "object",
		"properties": s.props,
	}
	if len(s.required) > 0 {
		obj["required"] = s.required
	}
	data, _ := json.Marshal(obj)
	return data
}
