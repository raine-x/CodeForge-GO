package llm

import (
	"encoding/json"
	"reflect"
	"testing"
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
			for _, provider := range []struct {
				name    string
				payload map[string]any
				want    string
			}{
				{"openai", (&OpenAIProvider{}).buildPayload(req), tc.openai},
				{"anthropic", (&AnthropicProvider{}).buildPayload(req), tc.anthropic},
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
