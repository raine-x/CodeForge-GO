package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/llm"
)

type imageRequestProvider struct {
	requests []llm.Request
}

func (p *imageRequestProvider) Name() string { return "image-test" }

func (p *imageRequestProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	req.Messages = cloneMessages(req.Messages)
	p.requests = append(p.requests, req)
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "answer"}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
}

func testImageMessage(data string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockImage, MediaType: "image/png", Data: data}}}
}

func TestRunWithImagesHistoryAndRegenerate(t *testing.T) {
	for _, input := range []string{"describe", ""} {
		t.Run("input="+input, func(t *testing.T) {
			p := &imageRequestProvider{}
			a := newEmitTestAgent(t, p)
			sess, err := a.history.Create("", "images")
			if err != nil {
				t.Fatal(err)
			}
			images := testImageMessage("aW1hZ2U=").Content
			var events []Event
			emit := func(ev Event) { events = append(events, ev) }
			if err := a.RunWithImages(context.Background(), sess.ID, input, images, emit); err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != 1 || len(sess.Messages) != 2 {
				t.Fatal("unexpected request or history count")
			}
			want := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: input}, images[0]}}}
			assertHistoryUnchanged(t, want, sess.Messages[:1])
			assertHistoryUnchanged(t, want, p.requests[0].Messages)
			if len(events) == 0 || events[0].Type != EventUser || events[0].Text != input {
				t.Fatal("user event must contain input text only")
			}
			data, err := json.Marshal(events)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), images[0].Data) {
				t.Fatal("image data leaked into events")
			}
			images[0].Data = "changed"
			assertHistoryUnchanged(t, want, sess.Messages[:1])
			delete(a.history.cache, sess.ID)
			loaded, ok := a.history.Get(sess.ID)
			if !ok {
				t.Fatal("history reload failed")
			}
			assertHistoryUnchanged(t, want, loaded.Messages[:1])
			loaded.Messages = append(loaded.Messages,
				llm.AssistantBlocksMessage([]llm.ContentBlock{{Type: llm.BlockToolUse, ID: "t1", Name: "read"}}),
				llm.ToolResultMessage("t1", "result", false),
				llm.TextMessage(llm.RoleAssistant, "old answer"))
			loaded.compressedUpTo = len(loaded.Messages)
			loaded.summaryText = "stale summary"
			loaded.SetLastUserInput("stale input")
			if err := a.Regenerate(context.Background(), sess.ID, emit); err != nil {
				t.Fatal(err)
			}
			if len(p.requests) != 2 {
				t.Fatal("regenerate did not issue one request")
			}
			assertHistoryUnchanged(t, want, p.requests[1].Messages)
			if len(loaded.Messages) != 2 || loaded.LastUserInput() != input {
				t.Fatal("regenerate did not restore real user turn")
			}
		})
	}
}

func TestRunTextDelegation(t *testing.T) {
	p := &imageRequestProvider{}
	a := newEmitTestAgent(t, p)
	sess, err := a.history.Create("", "text")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), sess.ID, "hello", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	assertHistoryUnchanged(t, []llm.Message{llm.TextMessage(llm.RoleUser, "hello")}, p.requests[0].Messages)
}

func TestEstimateImageTokensFixed(t *testing.T) {
	for _, data := range []string{"", "aW1hZ2U=", strings.Repeat("a", 1000000)} {
		msg := testImageMessage(data)
		msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockText, Text: "text"})
		if got := EstimateTokens([]llm.Message{msg}); got != 4101 {
			t.Fatalf("one image estimate = %d", got)
		}
		msg.Content = append(msg.Content, msg.Content[0])
		if got := EstimateTokens([]llm.Message{msg}); got != 8197 {
			t.Fatalf("two image estimate = %d", got)
		}
	}
}

func TestImageUserBoundaries(t *testing.T) {
	image := testImageMessage("aW1hZ2U=")
	if !isPlainUserText(image) {
		t.Fatal("image-only user must be a real user turn")
	}
	mixed := cloneMessages([]llm.Message{image})[0]
	mixed.Content = append(mixed.Content, llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: "t1", Content: "result"})
	if isPlainUserText(mixed) {
		t.Fatal("tool result cannot be a real user turn")
	}
	msgs := append(buildTurns(2, 10), image)
	msgs = append(msgs, llm.TextMessage(llm.RoleAssistant, "answer"))
	if got := chooseSplit(msgs, 1); got != 8 {
		t.Fatalf("split = %d, want 8", got)
	}
}

func TestImageSummaryOmitsData(t *testing.T) {
	image := testImageMessage("aW1hZ2U=")
	p := &ctxStubProvider{reply: "summary"}
	a := newCtxAgent(config.AgentConfig{ContextTokenBudget: 5000}, config.LLMConfig{}, p)
	sess := &Session{ID: "summary", Messages: []llm.Message{image, llm.TextMessage(llm.RoleAssistant, "answer"), image}}
	before := cloneMessages(sess.Messages)
	view := a.prepareMessages(context.Background(), sess, nil)
	if sess.compressedUpTo != 2 || len(p.prompts) != 1 {
		t.Fatal("image-only history was not summarized at user boundary")
	}
	if !strings.Contains(p.prompts[0], "图像内容已省略") || strings.Contains(p.prompts[0], image.Content[0].Data) {
		t.Fatal("summary must explicitly omit image data")
	}
	assertHistoryUnchanged(t, []llm.Message{image}, view[1:])
	assertHistoryUnchanged(t, before, sess.Messages)
}

func TestCompressPreservesCurrentImagesAcrossToolLoop(t *testing.T) {
	msgs := []llm.Message{testImageMessage("old"), llm.TextMessage(llm.RoleAssistant, "old answer"), testImageMessage("current")}
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			llm.AssistantBlocksMessage([]llm.ContentBlock{{Type: llm.BlockToolUse, ID: string(rune('a' + i)), Name: "read"}}),
			llm.ToolResultMessage(string(rune('a'+i)), strings.Repeat("result", 1000), false))
	}
	before := cloneMessages(msgs)
	out := Compress(msgs, 100)
	assertHistoryUnchanged(t, before, msgs)
	assertToolPairing(t, out)
	found := 0
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage {
				found++
				if b.Data != "current" {
					t.Fatal("old image was not compressed")
				}
			}
		}
	}
	if found != 1 || EstimateTokens(out) <= 100 {
		t.Fatal("current image must survive even when over budget")
	}
	assertHistoryUnchanged(t, out, Compress(out, 100))
	short := []llm.Message{testImageMessage("old"), llm.TextMessage(llm.RoleUser, "next")}
	view := Compress(short, 100)
	if view[0].Content[0].Type != llm.BlockText || !strings.Contains(view[0].Content[0].Text, "图像已省略") {
		t.Fatal("old image placeholder missing")
	}
	if short[0].Content[0].Data != "old" {
		t.Fatal("old image history mutated")
	}
}

func TestRunWithImagesOverBudget(t *testing.T) {
	p := &imageRequestProvider{}
	a := newEmitTestAgent(t, p)
	a.cfg.ContextTokenBudget = requestOverhead(a.systemPrompt(""), a.registry.DefinitionsFor(nil)) + 1000
	sess, err := a.history.Create("", "budget")
	if err != nil {
		t.Fatal(err)
	}
	var done bool
	err = a.RunWithImages(context.Background(), sess.ID, "", testImageMessage("current").Content, func(ev Event) { done = done || ev.Type == EventDone })
	if err == nil || !strings.Contains(err.Error(), "压缩后仍超过上下文预算") {
		t.Fatalf("expected context budget error, got %v", err)
	}
	if len(p.requests) != 0 || done {
		t.Fatal("over-budget images must not be sent or reported successful")
	}
	if sess.Messages[0].Content[1].Data != "current" {
		t.Fatal("current image history lost")
	}
}
