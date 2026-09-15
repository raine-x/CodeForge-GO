package llm

import (
	"context"
	"encoding/json"
	"io"
	"strings"

	"codeforge/config"
)

const (
	defaultAnthropicBase = "https://api.anthropic.com"
	anthropicVersion     = "2023-06-01"
)

// AnthropicProvider 适配 Anthropic /v1/messages（SSE 流式）。
type AnthropicProvider struct {
	baseURL string
	apiKey  string
	model   string
	retry   RetryPolicy
}

// NewAnthropic 构造 Anthropic 适配器。
func NewAnthropic(cfg config.LLMConfig) *AnthropicProvider {
	base := strings.TrimSpace(cfg.BaseURL)
	if base == "" {
		base = defaultAnthropicBase
	}
	return &AnthropicProvider{
		baseURL: strings.TrimRight(base, "/"),
		apiKey:  cfg.APIKey,
		model:   cfg.Model,
		retry:   retryPolicyFromConfig(cfg),
	}
}

// Name 实现 Provider。
func (p *AnthropicProvider) Name() string { return "anthropic" }

// Stream 实现 Provider。
func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	payload := p.buildPayload(req)
	headers := map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": anthropicVersion,
	}
	resp, err := postJSON(ctx, p.baseURL+"/v1/messages", headers, payload, p.retry)
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

func (p *AnthropicProvider) buildPayload(req Request) map[string]any {
	system := req.System
	messages := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if m.Role == RoleSystem {
			for _, b := range m.Content {
				if b.Type == BlockText && b.Text != "" {
					if system != "" {
						system += "\n\n"
					}
					system += b.Text
				}
			}
			continue
		}
		if blocks := convertAnthropicBlocks(m); len(blocks) > 0 {
			messages = append(messages, map[string]any{
				"role":    string(m.Role),
				"content": blocks,
			})
		}
	}

	payload := map[string]any{
		"model":      p.model,
		"max_tokens": clampMaxTokens(req.MaxTokens),
		"stream":     true,
		"messages":   messages,
	}
	if system != "" {
		payload["system"] = system
	}
	// 扩展思考：开启后 Anthropic 要求 temperature=1 且 max_tokens > budget_tokens
	// 预算值按 ThinkingSpec 规整（数字优先，兼容旧枚举 low/medium/high）
	if budget := specBudget(req.Thinking); budget > 0 {
		payload["thinking"] = map[string]any{
			"type":          "enabled",
			"budget_tokens": budget,
		}
		payload["temperature"] = 1
		if clampMaxTokens(req.MaxTokens) <= budget {
			payload["max_tokens"] = budget + 4096
		}
	} else if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, d := range req.Tools {
			schema := d.InputSchema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{
				"name":         d.Name,
				"description":  d.Description,
				"input_schema": json.RawMessage(schema),
			})
		}
		payload["tools"] = tools
	}
	return payload
}

// convertAnthropicBlocks 将统一消息内容转换为 Anthropic content blocks。
func convertAnthropicBlocks(m Message) []map[string]any {
	blocks := make([]map[string]any, 0, len(m.Content))
	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    b.ID,
				"name":  b.Name,
				"input": json.RawMessage(input),
			})
		case BlockToolResult:
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": b.ToolUseID,
				"content":     b.Content,
				"is_error":    b.IsError,
			})
		}
	}
	return blocks
}

type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Block struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	// message_start 的 message.usage 携带输入用量（含缓存三段）
	Message struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	// message_delta 的顶层 usage 携带输出用量（随流递增，取最后一次）
	Usage *anthropicUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// anthropicUsage 是 Anthropic 流里的用量片段。
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (p *AnthropicProvider) consume(ctx context.Context, r io.Reader, out chan<- StreamEvent) {
	toolIndex := map[int]string{}
	var usage *Usage // message_start 建立输入侧，message_delta 补输出侧

	err := scanSSE(r, func(_ string, data string) {
		if data == "" {
			return
		}
		var ev anthropicEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return
		}
		switch ev.Type {
		case "message_start":
			u := ev.Message.Usage
			usage = &Usage{
				InputTokens:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
				CachedTokens: u.CacheReadInputTokens,
			}
		case "content_block_start":
			if ev.Block.Type == BlockToolUse {
				toolIndex[ev.Index] = ev.Block.ID
				send(ctx, out, StreamEvent{
					Type:      EventToolUseStart,
					ToolUseID: ev.Block.ID,
					ToolName:  ev.Block.Name,
				})
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					send(ctx, out, StreamEvent{Type: EventTextDelta, Text: ev.Delta.Text})
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					send(ctx, out, StreamEvent{Type: EventReasoningDelta, Text: ev.Delta.Thinking})
				}
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					send(ctx, out, StreamEvent{
						Type:       EventToolUseDelta,
						ToolUseID:  toolIndex[ev.Index],
						InputDelta: ev.Delta.PartialJSON,
					})
				}
			}
		case "content_block_stop":
			if id, ok := toolIndex[ev.Index]; ok {
				send(ctx, out, StreamEvent{Type: EventToolUseStop, ToolUseID: id})
				delete(toolIndex, ev.Index)
			}
		case "message_delta":
			// 输出用量随流递增，只取最后一次的值
			if ev.Usage != nil && usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			if ev.Error != nil {
				send(ctx, out, StreamEvent{Type: EventError, Error: ev.Error.Message})
			}
		}
	})
	if err != nil {
		send(ctx, out, StreamEvent{Type: EventError, Error: err.Error()})
	}
	// 用量在流结束时统一上报（message_stop 之前），上层按会话累计
	if usage != nil {
		send(ctx, out, StreamEvent{Type: EventUsage, Usage: usage})
	}
	send(ctx, out, StreamEvent{Type: EventMessageStop})
}
