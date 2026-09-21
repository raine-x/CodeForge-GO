package llm

import "strings"

// ThinkingStep 是离散思考档位。
type ThinkingStep struct {
	Value string `json:"value"` // 请求参数原值（如 minimal / low）
	Label string `json:"label"` // 界面显示名
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

// OffThinkingValue 是「关闭」档的前端取值：选中它 = 不向上游发送任何思考参数。
const OffThinkingValue = "none"

// ThinkingSpecFor 按上游协议返回思考分级定义。
// 分级取自各厂商官方参数，而非本地固定三档：
//   - OpenAI 系（含旧 custom 兼容端点别名）：reasoning_effort ∈ minimal/low/medium/high，
//     外加「关闭」档（none，不上送参数，兼容不支持该参数的非推理模型）
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
				{Value: OffThinkingValue, Label: "关闭"},
				{Value: "minimal", Label: "极简"},
				{Value: "low", Label: "低"},
				{Value: "medium", Label: "中"},
				{Value: "high", Label: "高"},
			},
			Default: "medium",
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
//   - 「关闭」→ 空串（调用方据此不发参数）
//   - 数字字符串（Anthropic budget）：0=关闭，其余夹紧到 [OnMin,Max] 并对齐步进
//   - 枚举字符串：仅当属于该 provider 的合法档位时保留
func (s ThinkingSpec) NormalizeThinking(v string) string {
	v = strings.TrimSpace(v)
	if s.Mode == "none" || v == "" || isOffThinking(v) {
		return ""
	}
	if s.Mode == "range" {
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
	// steps：校验枚举（关闭值已在上面返回空串 = 不发参数）
	for _, st := range s.Steps {
		if strings.EqualFold(st.Value, v) {
			return st.Value
		}
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
