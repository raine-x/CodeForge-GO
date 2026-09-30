// Package llm 提供大模型统一适配层：统一消息模型、流式事件与 Tool Call 规范。
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codeforge/config"
)

// Role 是消息角色。
type Role string

const (
	// RoleSystem 系统消息。
	RoleSystem Role = "system"
	// RoleUser 用户消息。
	RoleUser Role = "user"
	// RoleAssistant 助手消息。
	RoleAssistant Role = "assistant"
)

// 内容块类型。
const (
	BlockImage      = "image"
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
	// BlockVideo 是一整段视频，**原样透传**给上游。
	//
	// 只对原生支持视频的模型有意义（Gemini / Qwen-VL 一类）：由上游自己抽帧、
	// 转写音轨并插入时间戳 —— 那是模型侧的解码能力，本地复现不了
	// （标准库没有任何视频解码器，而项目要求零第三方依赖 + CGO_ENABLED=0
	// 静态编译，见 AGENTS.md）。因此本项目**不做本地抽帧**：
	// 不支持视频的模型走能力门提示忽略，不做降级采样。
	//
	// 复用 ContentBlock 的 MediaType / Data（base64），不另立字段。
	BlockVideo = "video"
)

// ContentBlock 是统一的内容块，覆盖文本、工具调用与工具结果。
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use id
	Name      string          `json:"name,omitempty"`        // tool_use name
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use 参数
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result 关联的 tool_use id
	Content   string          `json:"content,omitempty"`     // tool_result 内容
	IsError   bool            `json:"is_error,omitempty"`    // tool_result 是否为错误
	// ThoughtSig 是上游附在函数调用上的「思考签名」，仅 tool_use 使用。
	//
	// Gemini 3 的 OpenAI 兼容层把它放在 tool call 的
	// extra_content.google.thought_signature，并要求后续请求**原样送回**：
	// 漏掉就直接 400 INVALID_ARGUMENT（Function call is missing a thought_signature），
	// 多轮工具调用彻底跑不动。它不是给人看的文本，只做透传，别去解析或改写。
	ThoughtSig string `json:"thought_sig,omitempty"`
}

// Message 是一条对话消息。
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`

	// Origin 标记这条消息的**来源**（空串 = 用户正常输入）。
	//
	// 对上游无意义（构造请求时只看 Role/Content），它解决的是本地问题：
	// 「运行中转向」注入的指令在历史里与正常提问长得一模一样，导致
	// 重新生成 / 编辑重发 / 回退可能定位到一句中途插话上，界面回放也分不清
	// 哪句是提问、哪句是插话。取值由 agent 层定义（见 agent.OriginSteer），
	// 本包只做透传与持久化。
	Origin string `json:"origin,omitempty"`
}

// TextMessage 构造一条纯文本消息。
func TextMessage(role Role, text string) Message {
	return Message{Role: role, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}

// AssistantBlocksMessage 构造一条含内容块的助手消息。
func AssistantBlocksMessage(blocks []ContentBlock) Message {
	return Message{Role: RoleAssistant, Content: blocks}
}

// ToolResultMessage 构造一条工具结果消息。
func ToolResultMessage(toolUseID, content string, isError bool) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{
		Type:      BlockToolResult,
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}}}
}

// ToolDef 是暴露给模型的工具定义。
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// StreamEventType 是流式事件类型。
type StreamEventType string

const (
	// EventTextDelta 文本增量。
	EventTextDelta StreamEventType = "text_delta"
	// EventReasoningDelta 思考/推理内容增量（OpenAI reasoning_content / Anthropic thinking）。
	EventReasoningDelta StreamEventType = "reasoning_delta"
	// EventToolUseStart 工具调用开始。
	EventToolUseStart StreamEventType = "tool_use_start"
	// EventToolUseDelta 工具参数增量。
	EventToolUseDelta StreamEventType = "tool_use_delta"
	// EventToolUseStop 工具调用结束。
	EventToolUseStop StreamEventType = "tool_use_stop"
	// EventMessageStop 本轮消息结束。
	EventMessageStop StreamEventType = "message_stop"
	// EventUsage 用量统计（每轮请求的流结束时发出一次，字段见 Usage）。
	EventUsage StreamEventType = "usage"
	// EventError 错误。
	EventError StreamEventType = "error"
)

// Usage 是一次请求的用量统计，由各适配器从流式响应解析、随 EventUsage 发出。
//
// 口径统一（两家上游换算到同一把尺子）：
//   - InputTokens  = 输入 tokens 总数（含缓存命中部分）；
//   - CachedTokens = 输入中命中上游提示缓存的部分（= 缓存命中）；
//   - 未命中 = InputTokens − CachedTokens；
//   - Anthropic 的 input_tokens 不含缓存三段，总数 = input + cache_creation + cache_read。
type Usage struct {
	InputTokens  int `json:"input_tokens"`  // 输入 tokens 总数（含缓存命中部分）
	CachedTokens int `json:"cached_tokens"` // 其中命中上游缓存的部分
	OutputTokens int `json:"output_tokens"` // 输出 tokens
}

// StreamEvent 是统一流式事件。
type StreamEvent struct {
	Type       StreamEventType `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	InputDelta string          `json:"input_delta,omitempty"`
	// ThoughtSig 随 EventToolUseDelta 携带：该工具调用的思考签名（分块流式时按到达顺序累加）。
	ThoughtSig string `json:"thought_sig,omitempty"`
	Error      string `json:"error,omitempty"`
	Usage      *Usage `json:"usage,omitempty"` // 仅 EventUsage 携带
}

// Request 是一次对话请求。
type Request struct {
	System      string
	Messages    []Message
	Tools       []ToolDef
	MaxTokens   int
	Temperature float64
	// Thinking 思考强度：low / medium / high（空字符串表示不启用）。
	Thinking string
}

// Provider 是大模型适配器接口。
type Provider interface {
	// Name 返回适配器名称。
	Name() string
	// Stream 发起流式对话，返回事件通道。
	Stream(ctx context.Context, req Request) (<-chan StreamEvent, error)
}

// ToolCall 是一次完整的工具调用（由流式事件拼装而成）。
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// ThoughtSig 见 ContentBlock.ThoughtSig：上游给的思考签名，必须原样回送。
	ThoughtSig string `json:"thought_sig,omitempty"`
}

// AssistantTurn 是模型一轮回复的汇总。
type AssistantTurn struct {
	Text      string         `json:"text"`
	ToolCalls []ToolCall     `json:"tool_calls"`
	Blocks    []ContentBlock `json:"blocks"`
	// ReasoningLen 是本轮收到的思考内容长度（仅用于判断「只回思考、没回正文」的空回合）。
	// 只记长度不存正文：思考正文已经实时推给前端，再留一份在内存里纯属重复。
	ReasoningLen int `json:"reasoning_len,omitempty"`
}

// NewProvider 按配置构造适配器。
func NewProvider(cfg config.LLMConfig) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case "", "anthropic":
		return NewAnthropic(cfg), nil
	case "openai", "custom": // custom 为历史别名（旧 local.yaml），实现与 openai 完全相同
		return NewOpenAI(cfg), nil
	default:
		return nil, fmt.Errorf("不支持的 LLM provider: %s", cfg.Provider)
	}
}

// clampMaxTokens 兜底 max_tokens。
func clampMaxTokens(n int) int {
	if n <= 0 {
		return 4096
	}
	return n
}

// thinkingBudget 将思考强度映射为 Anthropic budget_tokens（0 表示不启用）。
func thinkingBudget(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low", "轻度":
		return 2048
	case "medium", "中度":
		return 4096
	case "high", "重度":
		return 7000
	default:
		return 0
	}
}

// specBudget 按上游规格规整思考预算：数字字符串直接夹紧到合法区间；
// 枚举字符串走旧映射；非法值返回 0（不启用）。
func specBudget(v string) int {
	spec := ThinkingSpecFor("anthropic")
	norm := spec.NormalizeThinking(v)
	if norm == "" {
		// 兼容旧枚举映射（low/medium/high → 预算值）
		if b := thinkingBudget(v); b > 0 {
			return b
		}
		return 0
	}
	if isDigits(norm) {
		return atoi(norm)
	}
	return 0
}

// reasoningEffort 将思考强度校验/规整为 OpenAI reasoning_effort（空表示不启用）。
func reasoningEffort(level string) string {
	spec := ThinkingSpecFor("openai")
	return spec.NormalizeThinking(level)
}

// retryPolicyFromConfig 由配置推导重试策略（未配置时在 postJSON 内取默认值）。
func retryPolicyFromConfig(cfg config.LLMConfig) RetryPolicy {
	return RetryPolicy{
		MaxAttempts: cfg.MaxAttempts,
		Mode:        cfg.RetryMode,
		Backoff:     time.Duration(cfg.RetryBackoffMs) * time.Millisecond,
	}
}
