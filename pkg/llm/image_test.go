package llm

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"codeforge/pkg/errs"
)

func TestImagePayloads(t *testing.T) {
	image := ContentBlock{Type: BlockImage, MediaType: "image/png", Data: "aW1hZ2U="}
	for _, tc := range []struct {
		name      string
		blocks    []ContentBlock
		openai    string
		anthropic string
	}{
		{
			name:      "mixed",
			blocks:    []ContentBlock{{Type: BlockText, Text: "before"}, image, {Type: BlockText, Text: "after"}},
			openai:    `[{"role":"user","content":[{"type":"text","text":"before"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}},{"type":"text","text":"after"}]}]`,
			anthropic: `[{"role":"user","content":[{"type":"text","text":"before"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},{"type":"text","text":"after"}]}]`,
		},
		{
			name:      "image_only",
			blocks:    []ContentBlock{image},
			openai:    `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]}]`,
			anthropic: `[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}}]}]`,
		},
		{
			name:      "text_only",
			blocks:    []ContentBlock{{Type: BlockText, Text: "before"}, {Type: BlockText, Text: "after"}},
			openai:    `[{"role":"user","content":"beforeafter"}]`,
			anthropic: `[{"role":"user","content":[{"type":"text","text":"before"},{"type":"text","text":"after"}]}]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{Messages: []Message{{Role: RoleUser, Content: tc.blocks}}}
			openaiPayload := (&OpenAIProvider{}).buildPayload(req)
			anthPayload, err := (&AnthropicProvider{}).buildPayload(req)
			if err != nil {
				t.Fatalf("anthropic buildPayload: %v", err)
			}
			for _, provider := range []struct {
				name    string
				payload map[string]any
				want    string
			}{
				{"openai", openaiPayload, tc.openai},
				{"anthropic", anthPayload, tc.anthropic},
			} {
				t.Run(provider.name, func(t *testing.T) {
					data, err := json.Marshal(provider.payload["messages"])
					if err != nil {
						t.Fatal(err)
					}
					var got, want any
					if err := json.Unmarshal(data, &got); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal([]byte(provider.want), &want); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("payload mismatch: %s", data)
					}
				})
			}
		})
	}
}

// anthropicPayload 是 buildPayload 的测试包装：这些用例都只关心 payload 内容，
// 不关心错误，统一在这里断言「不该有错」比在每个调用点写一遍 if err 干净。
func anthropicPayload(t *testing.T, p *AnthropicProvider, req Request) map[string]any {
	t.Helper()
	payload, err := p.buildPayload(req)
	if err != nil {
		t.Fatalf("buildPayload 不该报错: %v", err)
	}
	return payload
}

// 视频走 OpenAI 兼容层：整段透传给上游，由上游自己抽帧/转写音轨。
func TestVideoPayloadPassthroughOpenAI(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
		{Type: BlockText, Text: "这段视频讲了什么"},
		{Type: BlockVideo, MediaType: "video/mp4", Data: "dmlkZW8="},
	}}}}
	data, err := json.Marshal((&OpenAIProvider{}).buildPayload(req)["messages"])
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"role":"user","content":[{"type":"text","text":"这段视频讲了什么"},` +
		`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,dmlkZW8="}}]}]`
	var got, wantAny any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantAny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantAny) {
		t.Fatalf("payload mismatch: %s", data)
	}
}

// Anthropic 没有视频内容块 —— 必须**显式报错**，绝不能静默丢弃。
//
// 这条是「静默失败」与「显式失败」的分界：原实现靠 switch 无 default
// 丢块，用户会以为模型看过了视频，实际它连一个字节都没收到。
func TestAnthropicRefusesVideoInsteadOfDroppingIt(t *testing.T) {
	req := Request{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
		{Type: BlockText, Text: "这段视频讲了什么"},
		{Type: BlockVideo, MediaType: "video/mp4", Data: "dmlkZW8="},
	}}}}
	_, err := (&AnthropicProvider{}).buildPayload(req)
	if err == nil {
		t.Fatal("Anthropic 收到视频块时必须报错，不能构造出一个把它丢掉的消息")
	}
	if got := errs.Classify(err); got != errs.KindUnsupportedMedia {
		t.Errorf("该错误应归为拒收媒体（agent 靠它记住该模型不支持视频），实际 %v", got)
	}
}

// Stream 层也必须把这个错误透出去：能力门记下「不支持」靠的就是它。
// 若 Stream 把它吞掉或换成别的错，记忆机制就静默失效了。
func TestAnthropicStreamSurfacesVideoRejection(t *testing.T) {
	p := &AnthropicProvider{baseURL: "https://example.invalid", apiKey: "k", model: "m"}
	_, err := p.Stream(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: []ContentBlock{
		{Type: BlockVideo, MediaType: "video/mp4", Data: "dmlkZW8="},
	}}}})
	if err == nil {
		t.Fatal("Stream 应在发起请求前就因视频块失败（此刻不该有任何网络动作）")
	}
	if got := errs.Classify(err); got != errs.KindUnsupportedMedia {
		t.Errorf("Stream 透出的错误应仍是 KindUnsupportedMedia，实际 %v", got)
	}
}

func TestContentBlockImageJSON(t *testing.T) {
	image := ContentBlock{Type: BlockImage, MediaType: "image/jpeg", Data: "aW1hZ2U="}
	data, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	var got ContentBlock
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, image) {
		t.Fatal("image fields lost during JSON round trip")
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["type"] != "image" || fields["media_type"] != image.MediaType || fields["data"] != image.Data {
		t.Fatalf("unexpected image JSON: %s", data)
	}
	data, err = json.Marshal(ContentBlock{Type: BlockText, Text: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"type":"text","text":"text"}` {
		t.Fatalf("unexpected text JSON: %s", data)
	}
}
