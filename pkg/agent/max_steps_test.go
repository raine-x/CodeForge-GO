package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/llm"
)

type maxStepsProvider func(context.Context, llm.Request) (<-chan llm.StreamEvent, error)

func (maxStepsProvider) Name() string { return "max-steps-stub" }

func (p maxStepsProvider) Stream(ctx context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	return p(ctx, req)
}

func maxStepsToolStream() <-chan llm.StreamEvent {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "working"}
	ch <- llm.StreamEvent{Type: llm.EventToolUseStart, ToolUseID: "call", ToolName: "missing_test_tool"}
	ch <- llm.StreamEvent{Type: llm.EventToolUseDelta, ToolUseID: "call", InputDelta: "{}"}
	close(ch)
	return ch
}

func TestMaxStepsSnapshotAndHistory(t *testing.T) {
	calls := 0
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		calls++
		return maxStepsToolStream(), nil
	}))
	sess, err := a.History().Create("", "limit")
	if err != nil {
		t.Fatal(err)
	}
	updated := make(chan struct{})
	update := make(chan struct{})
	go func() {
		<-update
		a.SetMaxSteps(1)
		close(updated)
	}()
	steps, done := 0, 0
	emit := func(ev Event) {
		if ev.Type == EventUser {
			close(update)
			<-updated
		}
		if ev.Type == EventStep {
			steps++
		}
		if ev.Type == EventDone {
			done++
		}
	}
	err = a.Run(context.Background(), sess.ID, "continue", emit)
	if err == nil || !strings.Contains(err.Error(), "3 轮") || !strings.Contains(err.Error(), "设置 > 常规") {
		t.Fatalf("expected limit and settings hint, got %v", err)
	}
	if calls != 3 || steps != 3 || done != 0 || a.MaxSteps() != 1 {
		t.Fatalf("calls=%d steps=%d done=%d max=%d", calls, steps, done, a.MaxSteps())
	}
	loaded, ok := NewHistory(a.history.st).Get(sess.ID)
	if !ok || len(loaded.Messages) != 7 || !reflect.DeepEqual(loaded.Messages, sess.Messages) {
		t.Fatal("full history was not persisted")
	}
	calls = 0
	err = a.Regenerate(context.Background(), sess.ID, func(ev Event) {
		if ev.Type == EventDone {
			t.Error("limit must not emit done")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "1 轮") || calls != 1 || len(sess.Messages) != 3 {
		t.Fatalf("regenerate did not use updated limit: calls=%d err=%v", calls, err)
	}
	for _, limit := range []int{100, 1} {
		a.SetMaxSteps(limit)
		calls = 0
		err = a.Run(context.Background(), sess.ID, "next", func(ev Event) {
			if ev.Type == EventDone {
				t.Error("limit must not emit done")
			}
		})
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%d 轮", limit)) || calls != limit {
			t.Fatalf("limit=%d calls=%d err=%v", limit, calls, err)
		}
	}
}

func TestMaxStepsSubagentCap(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return maxStepsToolStream(), nil
	}))
	for _, tc := range []struct{ parent, child int }{{1, 1}, {8, 8}, {100, 8}, {0, 8}} {
		a.SetMaxSteps(tc.parent)
		child := a.newSubagent("explore")
		steps := 0
		err := child.runLoopEphemeral(context.Background(), &Session{ID: "child"}, func(ev Event) {
			if ev.Type == EventStep {
				steps++
			}
			if ev.Type == EventDone {
				t.Error("exhausted child must not emit done")
			}
		})
		if child.MaxSteps() != tc.child || steps != tc.child || err == nil {
			t.Fatalf("parent=%d child=%d steps=%d err=%v", tc.parent, child.MaxSteps(), steps, err)
		}
	}
}

func TestMaxStepsCancellation(t *testing.T) {
	for _, stage := range []string{"before", "stream", "last_tool"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
				calls++
				if stage == "stream" {
					cancel()
					return make(chan llm.StreamEvent), nil
				}
				return maxStepsToolStream(), nil
			}))
			a.SetMaxSteps(1)
			if stage == "before" {
				cancel()
			}
			finished := make(chan error, 1)
			done := false
			go func() {
				finished <- a.runLoopEphemeral(ctx, &Session{ID: "cancel"}, func(ev Event) {
					if ev.Type == EventToolResult && stage == "last_tool" {
						cancel()
					}
					done = done || ev.Type == EventDone
				})
			}()
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) || done || (stage == "before" && calls != 0) {
					t.Fatalf("calls=%d done=%v err=%v", calls, done, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancellation blocked")
			}
		})
	}
}

func TestMaxStepsNormalCompletion(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		ch := make(chan llm.StreamEvent, 1)
		ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "complete"}
		close(ch)
		return ch, nil
	}))
	a.SetMaxSteps(1)
	done := 0
	err := a.runLoopEphemeral(context.Background(), &Session{ID: "complete"}, func(ev Event) {
		if ev.Type == EventDone {
			done++
		}
	})
	if err != nil || done != 1 {
		t.Fatalf("done=%d err=%v", done, err)
	}
}
