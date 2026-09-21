package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"codeforge/pkg/llm"
)

// 打断后用「断点重试」续上：服务端应保留原提问重跑，而不是要求用户重打一遍。
//
// 覆盖三件事：
//  1. 打断收尾后下发的 checkpoints 帧带 retry_back=0（前端据此挂重试圆环）；
//  2. retry 事件流里的 edit 帧文本 == 用户原话（不能被空文本覆盖）；
//  3. 重跑完成后 retry_back 变回 -1（这一轮完整结束了，圆环该摘掉）。
func TestRetryAfterInterruptKeepsUserText(t *testing.T) {
	provider := &gateProvider{gates: make(chan chan string, 4)}
	deps := newTestDepsAtProvider(t, "", provider)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))

	const question = "被打断的提问"

	// 第一轮：发出后卡在模型请求上，模拟「还没回复就被打断」。
	sendWS(t, conn, map[string]any{"type": "user_message", "text": question})
	waitGate(t, provider)
	frames := collectUntil(t, conn, "busy")
	sessionID := frameValue(frames, "session_id")
	if sessionID == "" {
		t.Fatal("没有从事件流里拿到会话 ID")
	}

	sendWS(t, conn, map[string]any{"type": "cancel"})
	idle := collectUntil(t, conn, "idle")
	if lastType(idle) != "idle" {
		t.Fatalf("打断后应收到 idle：%v", types(idle))
	}

	// 收尾的 checkpoints 必须指出「这一轮没跑完」。
	cp := collectUntil(t, conn, "checkpoints")
	if back, _ := findCheckpoint(cp, sessionID); back != 0 {
		t.Fatalf("未完成轮次应给 retry_back=0，得到 %v", back)
	}

	// 断点重试：不改文本，服务端按原提问重跑。
	sendWS(t, conn, map[string]any{
		"type": "retry", "session_id": sessionID, "back": 0, "rollback_files": true,
	})
	waitGate(t, provider) <- "重试后的回复"
	retryFrames := collectUntil(t, conn, "edit")
	var editText string
	for _, f := range retryFrames {
		if f["type"] == "edit" {
			editText, _ = f["text"].(string)
		}
	}
	if editText != question {
		t.Errorf("edit 帧应回放原提问 %q，得到 %q", question, editText)
	}

	// 放行：重跑这一轮正常收尾。
	collectUntil(t, conn, "idle")
	done := collectUntil(t, conn, "checkpoints")
	if back, ok := findCheckpoint(done, sessionID); ok && back != -1 {
		t.Errorf("已完成的轮次 retry_back 应为 -1，得到 %v", back)
	}
}

// errProvider 每次请求都立刻失败（绕过 HTTP 重试，保持测试快速）。
type errProvider struct{}

func (errProvider) Name() string { return "err-stub" }
func (errProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	return nil, errors.New("模拟上游故障")
}

// 重跑失败时也必须回放 edit 帧：截断已经发生，不补这一帧用户的提问就会
// 从屏幕上消失，只剩错误信息——断点重试的意义就是保住提问。
func TestRetryEmitsEditEvenWhenRunFails(t *testing.T) {
	deps := newTestDepsAtProvider(t, "", &errProvider{})
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))

	const question = "注定失败的提问"
	sendWS(t, conn, map[string]any{"type": "user_message", "text": question})
	first := collectUntil(t, conn, "checkpoints")
	sessionID := frameValue(first, "session_id")
	if sessionID == "" {
		t.Fatal("没有从事件流里拿到会话 ID")
	}

	// 这一轮因为上游故障没跑完：服务端应当给出可重试锚点。
	if back, _ := findCheckpoint(first, sessionID); back != 0 {
		t.Fatalf("失败轮次应给 retry_back=0，得到 %v", back)
	}

	// 重试同样会失败，但 edit 帧必须照发。
	sendWS(t, conn, map[string]any{
		"type": "retry", "session_id": sessionID, "back": 0, "rollback_files": true,
	})
	frames := collectUntil(t, conn, "idle")

	var editText string
	sawEdit := false
	for _, f := range frames {
		if f["type"] == "edit" {
			sawEdit = true
			editText, _ = f["text"].(string)
		}
	}
	if !sawEdit {
		t.Fatalf("重跑失败时也要下发 edit 帧回放视图，收到的却是：%v", types(frames))
	}
	if editText != question {
		t.Errorf("edit 帧应回放原提问 %q，得到 %q", question, editText)
	}
	// 失败的轮次仍然可重试：圆环不该因为一次失败就消失。
	if back, ok := findCheckpoint(frames, sessionID); ok && back != 0 {
		t.Errorf("失败后 retry_back 应仍为 0，得到 %v", back)
	}
}

// findCheckpoint 在帧序列里找属于本会话的 checkpoints 帧，返回它的 retry_back。
// 第二个返回值表示「是否找到过 checkpoints 帧」（没找到说明收尾事件丢了）。
func findCheckpoint(frames []map[string]any, sessionID string) (int, bool) {
	for _, f := range frames {
		if f["type"] != "checkpoints" {
			continue
		}
		if sid, _ := f["session_id"].(string); sid != sessionID && sid != "" {
			continue
		}
		back, ok := f["retry_back"].(float64)
		if !ok {
			return -2, true
		}
		return int(back), true
	}
	return -2, false
}
