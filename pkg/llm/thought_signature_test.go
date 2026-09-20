package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"codeforge/config"
)

// 思考签名（Gemini 3 的 OpenAI 兼容层）必须**收下来、存进历史、原样送回**。
// 漏送的后果不是报错在原地，而是下一轮 400 INVALID_ARGUMENT：
// 整条多轮工具链断在「看起来一切正常」的第二次请求上。

func chunkWith(extra string) string {
	return "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{" +
		"\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{}\"}" + extra +
		"}]}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
}

func TestOpenAIConsumeCapturesNestedThoughtSignature(t *testing.T) {
	body := chunkWith(`,"extra_content":{"google":{"thought_signature":"Sig-Nested"}}`)
	var got []string
	for _, ev := range runConsume(t, body) {
		if ev.ThoughtSig != "" {
			got = append(got, ev.ThoughtSig)
		}
	}
	if len(got) != 1 || got[0] != "Sig-Nested" {
		t.Fatalf("应收到嵌套位置的签名，实际 %q", got)
	}
}

// 自建网关常把签名平铺在 tool call 上，只认官方嵌套会静默丢签名。
func TestOpenAIConsumeCapturesFlatThoughtSignature(t *testing.T) {
	body := chunkWith(`,"thought_signature":"Sig-Flat"`)
	var got string
	for _, ev := range runConsume(t, body) {
		if ev.ThoughtSig != "" {
			got += ev.ThoughtSig
		}
	}
	if got != "Sig-Flat" {
		t.Fatalf("应收到平铺位置的签名，实际 %q", got)
	}
}

// 分块到达时按到达顺序累加（签名是 base64，可能被拆进多个 delta）。
func TestOpenAIConsumeAccumulatesChunkedSignature(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\"," +
		"\"extra_content\":{\"google\":{\"thought_signature\":\"AAAB\"}}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0," +
		"\"extra_content\":{\"google\":{\"thought_signature\":\"BBBB\"}}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	var got string
	for _, ev := range runConsume(t, body) {
		got += ev.ThoughtSig
	}
	if got != "AAABBBBB" {
		t.Fatalf("分块签名应累加为 AAABBBBB，实际 %q", got)
	}
}

func TestOpenAIPayloadResendsThoughtSignature(t *testing.T) {
	p := NewOpenAI(config.LLMConfig{Model: "gemini-3-pro"})
	msg := Message{Role: RoleAssistant, Content: []ContentBlock{{
		Type:       BlockToolUse,
		ID:         "call_1",
		Name:       "read_file",
		Input:      json.RawMessage(`{"path":"a.txt"}`),
		ThoughtSig: "Sig-Round-Trip",
	}}}
	data, err := json.Marshal(p.buildPayload(Request{Messages: []Message{msg}}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"extra_content":{"google":{"thought_signature":"Sig-Round-Trip"}}`) {
		t.Fatalf("回送载荷缺少签名的规定位置：\n%s", s)
	}

	// 没有签名的调用不该带上这个字段（OpenAI 官方端点会拒绝未知字段）。
	data, _ = json.Marshal(p.buildPayload(Request{Messages: []Message{
		{Role: RoleAssistant, Content: []ContentBlock{{
			Type: BlockToolUse, ID: "call_2", Name: "list_dir", Input: json.RawMessage(`{}`),
		}}},
	}}))
	if strings.Contains(string(data), "extra_content") {
		t.Errorf("无签名的调用不该带 extra_content：%s", data)
	}
}

// 会话历史是按 ContentBlock 的 JSON 落库的（SQLite messages.content）。
// 字段一旦在序列化中丢失，「重启后续不上签名」就会在下一轮复现同一个 400。
func TestThoughtSignatureSurvivesHistoryRoundTrip(t *testing.T) {
	raw, err := json.Marshal([]ContentBlock{{
		Type: BlockToolUse, ID: "c1", Name: "read_file",
		Input: json.RawMessage(`{"path":"a"}`), ThoughtSig: "Sig-Persisted",
	}})
	if err != nil {
		t.Fatal(err)
	}
	var back []ContentBlock
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != 1 || back[0].ThoughtSig != "Sig-Persisted" {
		t.Fatalf("签名未随历史往返保留：%+v", back)
	}
}

// 签名落点不写死：官方嵌套 / extra_content 直下 / 厂商命名层级 / 平铺 / 缺失。
func TestExtractThoughtSignatureAllPlacements(t *testing.T) {
	cases := []struct {
		name, extra, flat, want string
	}{
		{"官方嵌套", `{"google":{"thought_signature":"S1"}}`, "", "S1"},
		{"extra_content 直下", `{"thought_signature":"S2"}`, "", "S2"},
		{"别的厂商命名层级", `{"x_ai":{"thought_signature":"S3"}}`, "", "S3"},
		{"平铺在调用上", ``, "S4", "S4"},
		{"平铺优先", `{"google":{"thought_signature":"S5"}}`, "S6", "S6"},
		{"确实没有", `{"google":{"other":"x"}}`, "", ""},
		{"畸形 JSON 不炸", `{"google":`, "", ""},
		{"空 extra_content", ``, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractThoughtSignature(json.RawMessage(c.extra), c.flat)
			if got != c.want {
				t.Errorf("want %q got %q", c.want, got)
			}
		})
	}
}

// 错误里附的自证信息必须能区分三种成因（没回送 / 上游没给 / 旧历史缺签名）。
func TestSignatureSummaryDistinguuresCauses(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[
		{"id":"a","type":"function","function":{}},
		{"id":"b","type":"function","function":{},"extra_content":{"google":{"thought_signature":"S"}}}]}]}`)
	if got := signatureSummary(body); !strings.Contains(got, "2 处函数调用") ||
		!strings.Contains(got, "1 处带签名") || !strings.Contains(got, "旧会话") {
		t.Errorf("历史缺签名的情形判断错误：%q", got)
	}
	if got := signatureSummary([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); !strings.Contains(got, "没有函数调用") {
		t.Errorf("无函数调用时应直说：%q", got)
	}
	all := []byte(`{"messages":[{"role":"assistant","tool_calls":[
		{"id":"a","type":"function","function":{},"extra_content":{"google":{"thought_signature":"S"}}}]}]}`)
	if got := signatureSummary(all); !strings.Contains(got, "全部带签名") {
		t.Errorf("全部带签名时应指向格式/位置问题：%q", got)
	}
}
