package agent

import (
	"encoding/json"
	"strings"

	"codeforge/pkg/llm"
)

// EstimateTokens 粗略估算消息 token 数（中文按字符数 / 3 计）。
func EstimateTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		for _, b := range m.Content {
			total += len([]rune(b.Text))
			total += len([]rune(b.Content))
			total += len(b.Input)
		}
		total += 4 // 角色等结构开销
	}
	return total/3 + 1
}

// Compress 在超出预算时压缩历史：
//  1. 将较早的工具结果内容替换为占位符；
//  2. 仍超预算则丢弃最旧轮次（保留最近若干条）。
//
// 压缩后会修正首条消息，避免以 tool_result 开头（部分模型 API 会拒绝）。
func Compress(msgs []llm.Message, budget int) []llm.Message {
	if budget <= 0 || EstimateTokens(msgs) <= budget {
		return msgs
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)

	const keepTail = 8
	for i := 0; i < len(out)-keepTail; i++ {
		for j := range out[i].Content {
			b := &out[i].Content[j]
			if b.Type == llm.BlockToolResult && len([]rune(b.Content)) > 400 {
				b.Content = "…（历史工具结果已省略以节省上下文）"
			}
		}
	}
	if EstimateTokens(out) <= budget {
		return out
	}

	const keepRecent = 6
	if len(out) > keepRecent {
		out = out[len(out)-keepRecent:]
	}
	// 丢弃开头的 tool_result 消息，保证消息序列合法。
	for len(out) > 0 && startsWithToolResult(out[0]) {
		out = out[1:]
	}
	return out
}

func startsWithToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return true
		}
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			return false
		}
	}
	return false
}

// MarshalMessages 便于调试：将消息序列化为可读 JSON。
func MarshalMessages(msgs []llm.Message) string {
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}
