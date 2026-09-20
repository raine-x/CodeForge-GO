package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"codeforge/config"
)

const defaultOpenAIBase = "https://api.openai.com/v1"

// OpenAIProvider 适配 OpenAI /chat/completions（SSE 流式）。
type OpenAIProvider struct {
	baseURL string
	apiKey  string
	model   string
	retry   RetryPolicy
}

// NewOpenAI 构造 OpenAI 适配器。
func NewOpenAI(cfg config.LLMConfig) *OpenAIProvider {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultOpenAIBase
	}
	return &OpenAIProvider{
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		retry:   retryPolicyFromConfig(cfg),
	}
}

// Name 实现 Provider。
func (p *OpenAIProvider) Name() string { return "openai" }

// Stream 实现 Provider。
func (p *OpenAIProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	payload := p.buildPayload(req)
	headers := map[string]string{}
	if p.apiKey != "" {
		headers["Authorization"] = "Bearer " + p.apiKey
	}
	resp, err := postJSON(ctx, p.baseURL+"/chat/completions", headers, payload, p.retry)
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		p.consume(ctx, resp.Body, out)
	}()
	return out, nil
}

// oaiFunction 是 OpenAI function 调用载荷。
type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
	// ExtraContent 回填上游的厂商私有字段（Gemini 的 thought_signature 走这里）。
	// 用 any 而不是固定结构：不同兼容网关的嵌套略有差异，原样回送最稳。
	ExtraContent any `json:"extra_content,omitempty"`
}

// thoughtSignatureKey 是思考签名在 OpenAI 兼容载荷里的固定路径：
// tool_calls[i].extra_content.google.thought_signature。
func thoughtSignatureKey(sig string) any {
	return map[string]any{"google": map[string]any{"thought_signature": sig}}
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

func (p *OpenAIProvider) buildPayload(req Request) map[string]any {
	messages := make([]oaiMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, oaiMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		messages = append(messages, convertOAIMessages(m)...)
	}

	payload := map[string]any{
		"model":       p.model,
		"messages":    messages,
		"stream":      true,
		"max_tokens":  clampMaxTokens(req.MaxTokens),
		"temperature": req.Temperature,
	}
	// 让上游在流的末块附带 usage（choices 为空、带 usage 字段），
	// 否则拿不到 prompt/completion/cached tokens，用量统计无从谈起。
	payload["stream_options"] = map[string]any{"include_usage": true}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, d := range req.Tools {
			schema := d.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        d.Name,
					"description": d.Description,
					"parameters":  json.RawMessage(schema),
				},
			})
		}
		payload["tools"] = tools
	}
	// 推理强度：按上游规格校验枚举（minimal/low/medium/high），非法值回退默认
	if effort := reasoningEffort(req.Thinking); effort != "" {
		payload["reasoning_effort"] = effort
	}
	return payload
}

// convertOAIMessages 将统一消息转换为 OpenAI 消息序列。
func convertOAIMessages(m Message) []oaiMessage {
	var text strings.Builder
	var parts []map[string]any
	hasImages := false
	var toolCalls []oaiToolCall
	var toolResults []oaiMessage

	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			text.WriteString(b.Text)
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case BlockImage:
			hasImages = true
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + b.MediaType + ";base64," + b.Data,
				},
			})
		case BlockToolUse:
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, oaiToolCall{
				ID:   b.ID,
				Type: "function",
				Function: oaiFunction{
					Name:      b.Name,
					Arguments: args,
				},
			})
			if b.ThoughtSig != "" {
				// 丢签名的后果是下一轮直接 400，工具链整条断掉，
				// 所以历史上拿到过就必须一路带回去（含从 SQLite 读回的旧消息）。
				n := len(toolCalls) - 1
				toolCalls[n].ExtraContent = thoughtSignatureKey(b.ThoughtSig)
			}
		case BlockToolResult:
			toolResults = append(toolResults, oaiMessage{
				Role:       "tool",
				Content:    b.Content,
				ToolCallID: b.ToolUseID,
			})
		}
	}

	role := string(m.Role)
	if role == string(RoleSystem) {
		role = "user"
	}

	var content any
	if hasImages {
		content = parts
	} else if text.Len() > 0 {
		content = text.String()
	}
	var out []oaiMessage
	if content != nil || len(toolCalls) > 0 {
		out = append(out, oaiMessage{Role: role, Content: content, ToolCalls: toolCalls})
	}
	out = append(out, toolResults...)
	return out
}

type oaiUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Reasoning string `json:"reasoning_content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				// ExtraContent 用 RawMessage 收：官方位置是
				// extra_content.google.thought_signature，但自建网关会把它挂在
				// 别的层级（甚至平铺在 tool call 上）。只认一种位置时，签名会被
				// **静默**丢掉 —— 表现为下一轮 400，且没有任何本地线索。
				// 因此这里按「找到就用」处理，见 extractThoughtSignature。
				ExtraContent json.RawMessage `json:"extra_content"`
				// 平铺位置。
				ThoughtSignature string `json:"thought_signature"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// include_usage=true 时末块（choices 为空）携带的用量
	Usage *oaiUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// extractThoughtSignature 从工具调用的原始载荷里取思考签名，不猜死位置。
//
// 已知有三种落点：官方 extra_content.google.thought_signature、
// extra_content 直下、以及平铺在 tool call 上。写死任一种，另一种就会
// 被静默丢掉，而它的表现是「下一轮才 400」—— 离因太远，几乎查不到。
func extractThoughtSignature(extra json.RawMessage, flat string) string {
	if strings.TrimSpace(flat) != "" {
		return flat
	}
	if len(extra) == 0 {
		return ""
	}
	var node map[string]any
	if err := json.Unmarshal(extra, &node); err != nil {
		return ""
	}
	// 先本层，再往里看一层（google / 其它厂商命名都覆盖）。
	if s, ok := node["thought_signature"].(string); ok && s != "" {
		return s
	}
	for _, v := range node {
		inner, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := inner["thought_signature"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func (p *OpenAIProvider) consume(ctx context.Context, r io.Reader, out chan<- StreamEvent) {
	ids := map[int]string{}
	names := map[int]string{}
	finish := ""
	done := false
	failed := false
	var usage *Usage // 部分兼容网关会在多个块带 usage，取最后一次

	err := scanSSE(r, func(_ string, data string) {
		if data == "[DONE]" {
			done = true
			return
		}
		if data == "" || done || failed {
			return
		}
		var chunk oaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return
		}
		if chunk.Error != nil {
			failed = true
			send(ctx, out, StreamEvent{Type: EventError, Error: chunk.Error.Message})
			return
		}
		if chunk.Usage != nil {
			cached := 0
			if chunk.Usage.PromptTokensDetails != nil {
				cached = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			usage = &Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				CachedTokens: cached,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Reasoning != "" {
				send(ctx, out, StreamEvent{Type: EventReasoningDelta, Text: ch.Delta.Reasoning})
			}
			if ch.Delta.Content != "" {
				send(ctx, out, StreamEvent{Type: EventTextDelta, Text: ch.Delta.Content})
			}
			for _, tc := range ch.Delta.ToolCalls {
				id, seen := ids[tc.Index]
				if !seen {
					id = tc.ID
					if id == "" {
						id = fmt.Sprintf("call_%d", tc.Index)
					}
					ids[tc.Index] = id
					names[tc.Index] = tc.Function.Name
					send(ctx, out, StreamEvent{Type: EventToolUseStart, ToolUseID: id, ToolName: tc.Function.Name})
				} else if tc.Function.Name != "" && names[tc.Index] == "" {
					names[tc.Index] = tc.Function.Name
					// ⚠️ 必须**补发**一次 ToolUseStart。
					//
					// OpenAI 兼容协议允许把 function.name 拆在后续 delta 里 —— 首个 delta
					// 可能只带 index/arguments（甚至只带 id）。上面 `if !seen` 那次已经把
					// ToolUseStart 发出去了，当时 ToolName 是空的。
					// 消费端 agent.go 只在 ToolName 非空时才回填名字，所以这里若只更新本地
					// map 而不补发事件，**落库的 tool_use 块就会没有 name**。
					// 后果：前端回放该会话历史时 toolLabel 拿到 undefined 抛异常，
					// 中断整次回放 → 侧栏高亮停在上一个会话（2026-09-19 实际故障）。
					send(ctx, out, StreamEvent{Type: EventToolUseStart, ToolUseID: id, ToolName: tc.Function.Name})
				}
				if tc.Function.Arguments != "" {
					send(ctx, out, StreamEvent{Type: EventToolUseDelta, ToolUseID: id, InputDelta: tc.Function.Arguments})
				}
				sig := extractThoughtSignature(tc.ExtraContent, tc.ThoughtSignature)
				if sig != "" {
					// 与参数增量同通道下发（ThoughtSig 字段），由消费端累加进该工具调用：
					// 签名本身是 base64，可能整块到达也可能分块到达，累加两种都成立。
					send(ctx, out, StreamEvent{Type: EventToolUseDelta, ToolUseID: id, ThoughtSig: sig})
				}
			}
			if ch.FinishReason == "tool_calls" {
				for _, id := range ids {
					send(ctx, out, StreamEvent{Type: EventToolUseStop, ToolUseID: id})
				}
				ids = map[int]string{}
			}
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	})
	if err != nil && !failed {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: err.Error()})
	}
	if !failed && finish == "" && !done {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: "上游流提前结束：未收到 finish_reason 或 [DONE]"})
	}
	// max_tokens 预算（思考 + 回答共享）被耗尽时上游会以 length 结束且无报错，
	// 不显式上报会被上层当作正常完成，表现为「思考到一半就停了」。
	// content_filter 同理：上游认为命中内容过滤而截断，不报错就会被当成正常完成。
	// 两者都置 failed：错误后不再补发正常 MessageStop，上层按异常结束处理。
	if finish == "length" {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: "输出因达到 max_tokens 上限被截断（思考与回答共享该预算），请在设置中调大模型条目的「输出上限」后重新应用"})
	} else if finish == "content_filter" {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: "输出被上游内容过滤截断（content_filter），本轮未正常完成"})
	}
	// 用量在流结束时统一上报（message_stop 之前），上层按会话累计
	if usage != nil {
		send(ctx, out, StreamEvent{Type: EventUsage, Usage: usage})
	}
	if !failed {
		send(ctx, out, StreamEvent{Type: EventMessageStop})
	}
}
