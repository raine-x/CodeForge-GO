package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"codeforge/config"
	"codeforge/pkg/errs"
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
	// rpm / gate 见 OpenAIProvider 的同名字段（客户端 RPM 节流）。
	rpm  int
	gate *rateLimiter
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
		rpm:     cfg.RPM,
		gate:    limiterFor(cfg.Model),
	}
}

// Name 实现 Provider。
func (p *AnthropicProvider) Name() string { return "anthropic" }

// Stream 实现 Provider。
func (p *AnthropicProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	payload, err := p.buildPayload(req)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": anthropicVersion,
	}
	reopen := func() (*http.Response, error) {
		return postJSON(ctx, p.baseURL+"/v1/messages", headers, payload, p.retry, p.gate, p.rpm)
	}
	resp, err := reopen()
	if err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 64)
	go func() {
		defer close(out)
		// 与 openai 共用同一套「未产出内容则重试」的兜底
		pumpStream(ctx, p.retry, resp, reopen,
			func(r io.Reader) (bool, error) { return p.consume(ctx, r, out) }, out)
	}()
	return out, nil
}

func (p *AnthropicProvider) buildPayload(req Request) (map[string]any, error) {
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
		blocks, err := convertAnthropicBlocks(m)
		if err != nil {
			return nil, err
		}
		if len(blocks) > 0 {
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
	return payload, nil
}

// convertAnthropicBlocks 将统一消息内容转换为 Anthropic content blocks。
//
// 对视频**显式报错**而不是跳过：Anthropic 的 content block 只有
// text / image / document / tool_use / tool_result，没有视频。
// 原实现靠 switch 没有 default 来「自然丢弃」未知块 —— 那正是让
// 「视频附加了但模型压根没收到」静默发生的入口。丢掉一个用户明确
// 附上的附件等于伪造「模型已经看过了」，比直接失败糟得多。
func convertAnthropicBlocks(m Message) ([]map[string]any, error) {
	blocks := make([]map[string]any, 0, len(m.Content))
	for _, b := range m.Content {
		switch b.Type {
		case BlockVideo:
			return nil, errs.NewUnsupportedMedia(
				"Anthropic 协议没有视频内容块，无法送出行 %s 的视频附件（%s）", m.Role, b.MediaType)
		case BlockText:
			if b.Text == "" {
				continue
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": b.Text})
		case BlockImage:
			blocks = append(blocks, map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": b.MediaType,
					"data":       b.Data,
				},
			})
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
	return blocks, nil
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
		StopReason  string `json:"stop_reason"` // message_delta 携带：end_turn/max_tokens/tool_use/…
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

// consume 读取并派发一次流式响应。返回值语义与 OpenAIProvider.consume 一致：
// emitted 表示本轮是否已产出内容（决定能否安全重试），err 为传输层故障。
func (p *AnthropicProvider) consume(ctx context.Context, r io.Reader, out chan<- StreamEvent) (bool, error) {
	toolIndex := map[int]string{}
	var usage *Usage // message_start 建立输入侧，message_delta 补输出侧
	var stopReason string
	failed := false
	emitted := false // 已产出内容 → 不可重试

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
				emitted = true // 已开始产出工具调用
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
					emitted = true
					send(ctx, out, StreamEvent{Type: EventTextDelta, Text: ev.Delta.Text})
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					emitted = true
					send(ctx, out, StreamEvent{Type: EventReasoningDelta, Text: ev.Delta.Thinking})
				}
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					emitted = true
					send(ctx, out, StreamEvent{
						Type:       EventToolUseDelta,
						ToolUseID:  toolIndex[ev.Index],
						InputDelta: ev.Delta.PartialJSON,
					})
				}
			}
		case "content_block_stop":
			if id, ok := toolIndex[ev.Index]; ok {
				emitted = true
				send(ctx, out, StreamEvent{Type: EventToolUseStop, ToolUseID: id})
				delete(toolIndex, ev.Index)
			}
		case "message_delta":
			// 输出用量随流递增，只取最后一次的值；stop_reason 记录结束原因
			if ev.Usage != nil && usage != nil {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
		case "error":
			if ev.Error != nil {
				failed = true
				send(ctx, out, StreamEvent{Type: EventError, Error: ev.Error.Message})
			}
		}
	})
	if err != nil && !failed {
		// 传输层故障：不在这里发 EventError，交给 pump 决定重试还是报错（同 openai.go）
		return emitted, err
	}
	// max_tokens 截断：不报错会被上层当作正常完成，表现为「思考/回答到一半就停了」
	if !failed && stopReason == "max_tokens" {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: "输出因达到 max_tokens 上限被截断（思考与回答共享该预算），请在设置中调大模型条目的「输出上限」后重新应用"})
	}
	// 没收到 message_delta（如连接被掐断）时不能伪装成正常结束
	if !failed && stopReason == "" {
		failed = true
		send(ctx, out, StreamEvent{Type: EventError, Error: "上游流提前结束：未收到 message_delta（stop_reason）"})
	}
	// 用量在流结束时统一上报（message_stop 之前），上层按会话累计
	if usage != nil {
		send(ctx, out, StreamEvent{Type: EventUsage, Usage: usage})
	}
	if !failed {
		send(ctx, out, StreamEvent{Type: EventMessageStop})
	}
	return emitted, nil
}
