package llm

import "strings"

// ThinkingStep 是离散思考档位。
type ThinkingStep struct {
	Value string `json:"value"` // 请求参数原值（none/minimal/low/…/max，或 default=不下发）
	Label string `json:"label"` // 界面显示名（首字母大写：None/Minimal/…/Max）
}

// ThinkingSpec 描述当前 provider 支持的思考强度分级，
// 前端据此动态渲染滑条（steps=离散档位 / range=连续区间 / none=不支持）。
type ThinkingSpec struct {
	Mode    string         `json:"mode"`
	Steps   []ThinkingStep `json:"steps,omitempty"`
	Min     int            `json:"min,omitempty"` // range 模式：滑条最小值（0 = 最左端是「关闭」档）
	Max     int            `json:"max,omitempty"` // range 模式：最大值
	Step    int            `json:"step,omitempty"`
	OnMin   int            `json:"on_min,omitempty"` // range 模式：开启思考后的最小预算（0 = 与 Min 相同）
	Default string         `json:"default,omitempty"`
	Param   string         `json:"param"` // 上游参数名：reasoning_effort / budget_tokens
}

// 思考档位的两个特殊取值。它们在**请求上不同**，这是 2026-09-29 拆开的原因：
//
//	OffThinkingValue     "none"    → 显式下发 reasoning_effort="none"，明确要求不推理。
//	                                 它是 OpenAI 官方枚举里的真实一档，不是「不设置」。
//	DefaultThinkingValue "default" → 完全不下发该参数，由上游按模型自己的默认走。
//	                                 各模型的默认档不同（gpt-5.1=none、gpt-5=medium），
//	                                 本程序不该替它们选。
//
// 拆开之前只有「关闭 = 不发参数」一档，于是「不想思考」和「想让模型自己定」不可表达。
const (
	OffThinkingValue     = "none"
	DefaultThinkingValue = "default"
)

// ThinkingSpecFor 按上游协议返回思考分级定义。
// 分级取自各厂商官方参数，而非本地固定三档：
//   - OpenAI 系（含旧 custom 兼容端点别名）：reasoning_effort ∈ none/minimal/low/
//     medium/high/xhigh/max（2026-09-29 补齐 xhigh / max 两档上限），外加一档
//     Default（default，不下发参数 = 走上游默认）
//   - Anthropic：thinking.budget_tokens 连续区间（0=关闭 / 1024–16384，步进 512）
func ThinkingSpecFor(provider string) ThinkingSpec {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic":
		return ThinkingSpec{
			Mode:    "range",
			Min:     0, // 滑条最左端 = 关闭（0 不发送 thinking 字段）
			OnMin:   1024,
			Max:     16384,
			Step:    512,
			Default: "4096",
			Param:   "budget_tokens",
		}
	case "openai", "custom": // custom 为历史别名，等价 openai
		return ThinkingSpec{
			Mode: "steps",
			Steps: []ThinkingStep{
				{Value: OffThinkingValue, Label: "None"},
				{Value: DefaultThinkingValue, Label: "Default"},
				{Value: "minimal", Label: "Minimal"},
				{Value: "low", Label: "Low"},
				{Value: "medium", Label: "Medium"},
				{Value: "high", Label: "High"},
				{Value: "xhigh", Label: "Xhigh"},
				{Value: "max", Label: "Max"},
			},
			// 非法值回落到 Default 而不是替用户猜一档：猜错就是把用户没要的
			// 思考强度加上去（还可能超出模型支持集）。不下发永远不会是错的。
			Default: DefaultThinkingValue,
			Param:   "reasoning_effort",
		}
	default:
		return ThinkingSpec{Mode: "none", Param: ""}
	}
}

// isOffThinking 判断请求值是否表示「关闭思考」（none/0/off/关闭…）。
func isOffThinking(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case OffThinkingValue, "0", "off", "false", "关闭", "不启用", "禁用":
		return true
	}
	return false
}

// NormalizeThinking 将前端上送的思考值规整为上游可用的参数值。
// 返回空串 = 调用方不向下游发该参数。两种协议对「关闭」的处理**不同**：
//   - steps（OpenAI）：none → 原样返回并显式下发（官方合法取值）；
//     Default 档 → 空串（不下发 = 走上游默认）。只有「完全没指定」和 Default 是空串。
//   - range（Anthropic）：关闭值（none/0/off/关闭…）→ 空串，不发 thinking 字段。
//     Anthropic 没有「none」这个取值，关闭就是不发这个字段。
func (s ThinkingSpec) NormalizeThinking(v string) string {
	v = strings.TrimSpace(v)
	if s.Mode == "none" || v == "" {
		return ""
	}
	if s.Mode == "range" {
		if isOffThinking(v) {
			return "" // 关闭：Anthropic 靠「不发 thinking 字段」表示
		}
		if !isDigits(v) {
			// 兼容旧枚举值（low/medium/high）
			return itoa(legacyBudget(v))
		}
		n := atoi(v)
		if n <= 0 {
			return "" // 0 = 关闭
		}
		onMin := s.OnMin
		if onMin <= 0 {
			onMin = s.Min
		}
		if n < onMin {
			n = onMin
		}
		if n > s.Max {
			n = s.Max
		}
		n = n - n%s.Step
		if n < onMin {
			n = onMin // 对齐步进后不得跌破开启下限
		}
		return itoa(n)
	}
	// steps：命中档位即按档位语义返回。
	// Default 档是唯一返回空串的合法档（不下发 = 走上游默认）；
	// none 档照常返回 "none"，由 openai.buildPayload 显式下发。
	for _, st := range s.Steps {
		if strings.EqualFold(st.Value, v) {
			if strings.EqualFold(st.Value, DefaultThinkingValue) {
				return ""
			}
			return st.Value
		}
	}
	// 旧值（0/off/关闭…）与关闭档同义，归到关闭档。
	if isOffThinking(v) {
		for _, st := range s.Steps {
			if strings.EqualFold(st.Value, OffThinkingValue) {
				return st.Value
			}
		}
	}
	// 非法值回落到 spec.Default。Default 档的语义是「不下发」，所以这里要解析成
	// 空串 —— 直接把 "default" 返回出去会被 openai.buildPayload 当成参数发出去。
	if strings.EqualFold(s.Default, DefaultThinkingValue) {
		return ""
	}
	return s.Default
}

func legacyBudget(level string) int {
	switch strings.ToLower(level) {
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

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
