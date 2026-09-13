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
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// ContentBlock 是统一的内容块，覆盖文本、工具调用与工具结果。
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use id
	Name      string          `json:"name,omitempty"`        // tool_use name
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use 参数
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result 关联的 tool_use id
	Content   string          `json:"content,omitempty"`     // tool_result 内容
	IsError   bool            `json:"is_error,omitempty"`    // tool_result 是否为错误
}

// Message 是一条对话消息。
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
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
	// EventError 错误。
	EventError StreamEventType = "error"
)

// StreamEvent 是统一流式事件。
type StreamEvent struct {
	Type       StreamEventType `json:"type"`
	Text       string          `json:"text,omitempty"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	ToolName   string          `json:"tool_name,omitempty"`
	InputDelta string          `json:"input_delta,omitempty"`
	Error      string          `json:"error,omitempty"`
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
}

// AssistantTurn 是模型一轮回复的汇总。
type AssistantTurn struct {
	Text      string     `json:"text"`
	ToolCalls []ToolCall `json:"tool_calls"`
	Blocks    []ContentBlock `json:"blocks"`
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
		Backoff:     time.Duration(cfg.RetryBackoffMs) * time.Millisecond,
	}
}
