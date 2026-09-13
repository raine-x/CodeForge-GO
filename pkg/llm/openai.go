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
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content,omitempty"`
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
	var toolCalls []oaiToolCall
	var toolResults []oaiMessage

	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			text.WriteString(b.Text)
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

	var out []oaiMessage
	if text.Len() > 0 || len(toolCalls) > 0 {
		out = append(out, oaiMessage{Role: role, Content: text.String(), ToolCalls: toolCalls})
	}
	out = append(out, toolResults...)
	return out
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
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (p *OpenAIProvider) consume(ctx context.Context, r io.Reader, out chan<- StreamEvent) {
	ids := map[int]string{}
	names := map[int]string{}
	finish := ""

	err := scanSSE(r, func(_ string, data string) {
		if data == "" || data == "[DONE]" {
			return
		}
		var chunk oaiChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return
		}
		if chunk.Error != nil {
			send(ctx, out, StreamEvent{Type: EventError, Error: chunk.Error.Message})
			return
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
				}
				if tc.Function.Arguments != "" {
					send(ctx, out, StreamEvent{Type: EventToolUseDelta, ToolUseID: id, InputDelta: tc.Function.Arguments})
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
	if err != nil {
		send(ctx, out, StreamEvent{Type: EventError, Error: err.Error()})
	}
	// max_tokens 预算（思考 + 回答共享）被耗尽时上游会以 length 结束且无报错，
	// 不显式上报会被上层当作正常完成，表现为「思考到一半就停了」。
	if finish == "length" {
		send(ctx, out, StreamEvent{Type: EventError, Error: "输出因达到 max_tokens 上限被截断（思考与回答共享该预算），请在设置中调大模型条目的「输出上限」后重新应用"})
	}
	send(ctx, out, StreamEvent{Type: EventMessageStop})
}
