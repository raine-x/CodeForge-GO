package llm

import (
	"encoding/json"
	"testing"

	"codeforge/config"
)

// 临时验证：思考强度参数是否真正进入厂商请求载荷。
func TestThinkingParams(t *testing.T) {
	levels := map[string]struct {
		anthropic map[string]any
		openai    any
	}{
		"low":    {nil, "low"},
		"medium": {nil, "medium"},
		"high":   {nil, "high"},
	}
	_ = levels

	// OpenAI 路径
	o := NewOpenAI(config.LLMConfig{Model: "gpt-5"})
	p := o.buildPayload(Request{Thinking: "high", MaxTokens: 100})
	if p["reasoning_effort"] != "high" {
		t.Fatalf("openai reasoning_effort = %v, want high", p["reasoning_effort"])
	}

	// Anthropic 路径
	a := NewAnthropic(config.LLMConfig{Model: "claude-sonnet-4-20250514"})
	ap := a.buildPayload(Request{Thinking: "medium", MaxTokens: 8192, Temperature: 0.2})
	th, ok := ap["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("anthropic thinking missing: %v", ap["thinking"])
	}
	if th["budget_tokens"] != 4096 {
		t.Fatalf("budget_tokens = %v, want 4096", th["budget_tokens"])
	}
	if ap["temperature"] != 1 {
		t.Fatalf("temperature = %v, want 1 (thinking 模式强制)", ap["temperature"])
	}
	b, _ := json.Marshal(ap["max_tokens"])
	t.Logf("anthropic max_tokens = %s (须 > budget)", b)

	// Anthropic 数字预算：直接夹紧到 [1024,16384] 并对齐步进
	apNum := a.buildPayload(Request{Thinking: "9999", MaxTokens: 8192})
	thNum := apNum["thinking"].(map[string]any)
	if thNum["budget_tokens"] != 9728 { // 9999 → 对齐 512 步进
		t.Fatalf("numeric budget = %v, want 9728", thNum["budget_tokens"])
	}

	// 低于开启下限：夹到 1024（而不是 0/512 这种非法值）
	apLow := a.buildPayload(Request{Thinking: "512", MaxTokens: 8192})
	if got := apLow["thinking"].(map[string]any)["budget_tokens"]; got != 1024 {
		t.Fatalf("512 应夹到开启下限 1024，实际 %v", got)
	}

	// OpenAI 枚举校验：非法值回退默认 medium
	oBad := NewOpenAI(config.LLMConfig{Model: "gpt-5"})
	pBad := oBad.buildPayload(Request{Thinking: "ultra"})
	if pBad["reasoning_effort"] != "medium" {
		t.Fatalf("invalid effort = %v, want fallback medium", pBad["reasoning_effort"])
	}

	// 关闭档：none/0/off/关闭 都不应携带思考参数（兼容不支持该参数的非推理模型）
	for _, off := range []string{"none", "0", "off", "关闭"} {
		if _, exists := oBad.buildPayload(Request{Thinking: off})["reasoning_effort"]; exists {
			t.Fatalf("openai 关闭档 %q 不应携带 reasoning_effort", off)
		}
		if _, exists := a.buildPayload(Request{Thinking: off, MaxTokens: 8192})["thinking"]; exists {
			t.Fatalf("anthropic 关闭档 %q 不应携带 thinking", off)
		}
	}

	// 不启用时不应带参数
	ap2 := a.buildPayload(Request{MaxTokens: 8192, Temperature: 0.2})
	if _, exists := ap2["thinking"]; exists {
		t.Fatal("未指定 thinking 时不应携带 thinking 字段")
	}
	if ap2["temperature"] != 0.2 {
		t.Fatalf("普通模式 temperature = %v, want 0.2", ap2["temperature"])
	}

	// 分级规格：随 provider 动态返回；关闭档在最左端（openai 首档 / anthropic Min=0）
	os := ThinkingSpecFor("openai")
	if os.Mode != "steps" || len(os.Steps) != 5 || os.Steps[0].Value != OffThinkingValue {
		t.Fatalf("openai spec = %+v, want 5 steps with off first", os)
	}
	as := ThinkingSpecFor("anthropic")
	if as.Mode != "range" || as.Min != 0 || as.OnMin != 1024 || as.Max != 16384 {
		t.Fatalf("anthropic spec = %+v, want range 0(off)/1024-16384", as)
	}
	if ThinkingSpecFor("unknown").Mode != "none" {
		t.Fatal("unknown provider should be none")
	}
}
