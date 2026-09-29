package llm

import (
	"encoding/json"
	"strings"
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

	// OpenAI 枚举校验：非法值回落到 Default 档（不下发参数），
	// 而不是替用户猜一档 —— 猜错就是把用户没要的思考强度加上去。
	oBad := NewOpenAI(config.LLMConfig{Model: "gpt-5"})
	pBad := oBad.buildPayload(Request{Thinking: "ultra"})
	if _, exists := pBad["reasoning_effort"]; exists {
		t.Fatalf("非法值应回落到 Default 档（不下发 reasoning_effort），实际 %v", pBad["reasoning_effort"])
	}

	// 关闭档：none 是官方合法取值，**要显式下发**（2026-09-29 起不再是「不发参数」）。
	// 不发参数的是 Default 档 —— 两者在请求上必须可区分。
	if got := oBad.buildPayload(Request{Thinking: "none"})["reasoning_effort"]; got != "none" {
		t.Fatalf("openai 关闭档应下发 reasoning_effort=none，实际 %v", got)
	}
	for _, d := range []string{"default", ""} {
		if _, exists := oBad.buildPayload(Request{Thinking: d})["reasoning_effort"]; exists {
			t.Fatalf("openai Default 档 %q 不应携带 reasoning_effort", d)
		}
	}
	// 旧值（0/off/关闭…）与关闭档同义，归到 none
	for _, legacy := range []string{"0", "off", "关闭"} {
		if got := oBad.buildPayload(Request{Thinking: legacy})["reasoning_effort"]; got != "none" {
			t.Fatalf("openai 旧关闭值 %q 应归到 none，实际 %v", legacy, got)
		}
		if _, exists := a.buildPayload(Request{Thinking: legacy, MaxTokens: 8192})["thinking"]; exists {
			t.Fatalf("anthropic 关闭值 %q 不应携带 thinking", legacy)
		}
	}
	// Anthropic 侧 none 仍表示「不发 thinking 字段」（该协议没有 none 取值）
	if _, exists := a.buildPayload(Request{Thinking: "none", MaxTokens: 8192})["thinking"]; exists {
		t.Fatal("anthropic 关闭档 none 不应携带 thinking")
	}

	// 新增档位 xhigh / max 必须能原样下发（此前上限只到 high，选了会被静默回退）
	for _, lvl := range []string{"minimal", "low", "medium", "high", "xhigh", "max"} {
		if got := oBad.buildPayload(Request{Thinking: lvl})["reasoning_effort"]; got != lvl {
			t.Fatalf("openai 档位 %q 应原样下发，实际 %v", lvl, got)
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

	// 分级规格：随 provider 动态返回。
	// 2026-09-29：openai 从 5 档扩到 8 档，补 minimal / xhigh / max 与 Default；
	// 档位集合必须与官方枚举一致 —— 少一档用户就选不到，多一档上游会 400。
	os := ThinkingSpecFor("openai")
	wantSteps := []string{"none", "default", "minimal", "low", "medium", "high", "xhigh", "max"}
	if os.Mode != "steps" || len(os.Steps) != len(wantSteps) {
		t.Fatalf("openai spec = %+v, want %d steps", os, len(wantSteps))
	}
	for i, w := range wantSteps {
		if os.Steps[i].Value != w {
			t.Fatalf("openai step[%d] = %q, want %q", i, os.Steps[i].Value, w)
		}
		if os.Steps[i].Label == "" {
			t.Fatalf("openai step[%d] (%s) 缺 label，界面会退回首字母大写", i, w)
		}
	}
	// label 首字母大写：None / Default / Minimal / … / Xhigh / Max
	for _, st := range os.Steps {
		if st.Label != strings.ToUpper(st.Label[:1])+st.Label[1:] {
			t.Fatalf("step %q 的 label %q 未首字母大写", st.Value, st.Label)
		}
	}
	as := ThinkingSpecFor("anthropic")
	if as.Mode != "range" || as.Min != 0 || as.OnMin != 1024 || as.Max != 16384 {
		t.Fatalf("anthropic spec = %+v, want range 0(off)/1024-16384", as)
	}
	if ThinkingSpecFor("unknown").Mode != "none" {
		t.Fatal("unknown provider should be none")
	}
}
