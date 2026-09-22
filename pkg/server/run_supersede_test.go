package server

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/pkg/llm"
)

// gateProvider 每次请求都停在闸门上，直到测试放行才回一段文本。
// 用来精确控制「某一轮什么时候收尾」，从而验证被取代的那一轮不会补发 idle。
type gateProvider struct {
	gates chan chan string
}

func (p *gateProvider) Name() string { return "gate-stub" }

func (p *gateProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	release := make(chan string)
	p.gates <- release
	ch := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(ch)
		select {
		case text := <-release:
			ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: text}
			ch <- llm.StreamEvent{Type: llm.EventMessageStop}
		case <-ctx.Done(): // 被打断的轮次不再产内容
		}
	}()
	return ch, nil
}

// 打断后立刻重发：上一轮是异步收尾的，它迟到的 idle 若照发，就会把刚起来的
// 新一轮标成「已空闲」—— 按钮恢复、思考条消失，看起来像任务凭空停了。
func TestSupersededRunDoesNotClobberNewRunState(t *testing.T) {
	provider := &gateProvider{gates: make(chan chan string, 4)}
	deps := newTestDepsAtProvider(t, "", provider)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)

	conn := steerDial(t, ts, steerJar(t, ts))

	// 第一轮：发出后停在模型请求上，此刻它一定还在跑。
	sendWS(t, conn, map[string]any{"type": "user_message", "text": "第一轮"})
	waitGate(t, provider)
	frames := collectUntil(t, conn, "busy")
	sessionID := frameValue(frames, "session_id")
	if sessionID == "" {
		t.Fatal("没有从事件流里拿到会话 ID")
	}

	// 第二轮：服务端先 stop 第一轮，第一轮随即异步收尾。
	sendWS(t, conn, map[string]any{"type": "user_message", "text": "第二轮"})
	gateB := waitGate(t, provider)

	// 栅栏：context 查询必然回一帧。旧轮如果补发 idle，一定落在栅栏之前。
	// （不能用「读窗口内没有消息」来断言 —— 读超时会把连接废掉，后续断言全瞎。）
	sendWS(t, conn, map[string]any{"type": "context", "session_id": sessionID})
	guarded := collectUntil(t, conn, "context")
	for _, f := range guarded {
		if f["type"] == "idle" {
			t.Fatalf("被取代的旧轮仍然下发了 idle，前端运行态会被抹掉：%v", types(guarded))
		}
	}

	// 放行第二轮：这一轮该正常收尾一次。
	gateB <- "第二轮的回复"
	ended := collectUntil(t, conn, "idle")
	if lastType(ended) != "idle" {
		t.Fatalf("新一轮没有收到 idle：%v", types(ended))
	}
}

// 打断后立刻重发：两轮会短暂共用同一个会话对象（run 的 stop 只 cancel、不 join）。
// 这条用例反复制造那个交叠窗口，断言落库的历史始终**结构完整、用户消息一条不丢**。
//
// ⚠️ 说明（2026-09-22）：本机没有 C 编译器，`go test -race` 不可用（-race 依赖 cgo），
// 所以这里只能做确定性压力 + 结构断言，**不能证明数据竞争不存在** —— 它拦的是
// 「已经表现成丢消息 / 顺序错乱」的回归。压力下没复现，就不去动 run 的并发模型
// （那属于另一件事，改动面比这次修复大得多）。
func TestConcurrentRunsKeepHistoryIntact(t *testing.T) {
	for round := 0; round < 5; round++ {
		provider := &gateProvider{gates: make(chan chan string, 8)}
		deps := newTestDepsAtProvider(t, "", provider)
		ts := httptest.NewServer(deps.newServer().Routes())

		conn := steerDial(t, ts, steerJar(t, ts))
		sendWS(t, conn, map[string]any{"type": "user_message", "text": "第一轮提问"})
		waitGate(t, provider)
		frames := collectUntil(t, conn, "busy")
		sessionID := frameValue(frames, "session_id")
		if sessionID == "" {
			t.Fatalf("第 %d 轮：没有拿到会话 ID", round)
		}

		// 打断 + 立刻重发（不等 idle）：制造两轮交叠的窗口。
		// ⚠️ 第二轮必须显式带上 session_id：不带会被 startUserMessage 当成
		// 「新建会话」，两轮落在不同会话里，这条用例就白跑了。
		sendWS(t, conn, map[string]any{"type": "cancel"})
		sendWS(t, conn, map[string]any{"type": "user_message", "session_id": sessionID, "text": "第二轮提问"})
		gate := waitGate(t, provider)
		gate <- "第二轮的回复"
		collectUntil(t, conn, "idle")

		sess, ok := deps.agent.History().Get(sessionID)
		if !ok {
			t.Fatalf("第 %d 轮：读不到会话", round)
		}
		if bad := historyProblem(sess.Messages, "第一轮提问", "第二轮提问"); bad != "" {
			t.Fatalf("第 %d 轮：历史被破坏 —— %s\n%v", round, bad, historyDigest(sess.Messages))
		}
		ts.Close()
	}
}

// historyProblem 检查消息序列的结构约束，返回问题描述（空串 = 没问题）。
//
// 盯三件事：用户消息一条不丢且顺序正确；tool_use 与 tool_result 一一配对；
// tool_use id 不重复 —— 后者是「两轮并发 append 同一份历史」最典型的症状。
func historyProblem(msgs []llm.Message, wants ...string) string {
	var users []string
	pending := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockText:
				if m.Role == llm.RoleUser {
					users = append(users, b.Text)
				}
			case llm.BlockToolUse:
				if pending[b.ID] {
					return "tool_use 重复出现（" + b.Name + "）"
				}
				pending[b.ID] = true
			case llm.BlockToolResult:
				if !pending[b.ToolUseID] {
					return "tool_result 找不到对应的 tool_use"
				}
				delete(pending, b.ToolUseID)
			}
		}
	}
	if len(pending) > 0 {
		return "有 tool_use 没有配对的结果"
	}
	if len(users) < len(wants) {
		return "用户消息丢条"
	}
	for i, w := range wants {
		if users[i] != w {
			return "用户消息顺序错乱"
		}
	}
	return ""
}

// historyDigest 把消息序列压成便于排障的一行摘要（角色 + 块类型 + 正文）。
func historyDigest(msgs []llm.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		tag := string(m.Role)
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				tag += "+use(" + b.Name + ")"
			case llm.BlockToolResult:
				tag += "+result"
			case llm.BlockText:
				if b.Text != "" {
					tag += ":" + b.Text
				}
			}
		}
		out = append(out, tag)
	}
	return out
}

func sendWS(t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	if err := conn.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}

func waitGate(t *testing.T, p *gateProvider) chan string {
	t.Helper()
	select {
	case g := <-p.gates:
		return g
	case <-time.After(5 * time.Second):
		t.Fatal("没有发起模型请求")
		return nil
	}
}

// collectUntil 收集事件帧，直到读到类型为 want 的那一帧为止（含）。
// 超时或读错都直接失败：这些帧都是本用例必然会产生出来的。
func collectUntil(t *testing.T, conn *websocket.Conn, want string) []map[string]any {
	t.Helper()
	var out []map[string]any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var ev map[string]any
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatalf("读取事件失败（已收到 %v）: %v", types(out), err)
		}
		out = append(out, ev)
		if ev["type"] == want {
			return out
		}
	}
	t.Fatalf("等待 %q 超时，期间只收到：%v", want, types(out))
	return nil
}

func types(frames []map[string]any) []string {
	out := make([]string, 0, len(frames))
	for _, f := range frames {
		s, _ := f["type"].(string)
		out = append(out, s)
	}
	return out
}

func lastType(frames []map[string]any) string {
	if len(frames) == 0 {
		return ""
	}
	s, _ := frames[len(frames)-1]["type"].(string)
	return s
}

// frameValue 取第一个带该字段的字符串值（会话 ID 由 session 帧带回）。
func frameValue(frames []map[string]any, key string) string {
	for _, f := range frames {
		if s, ok := f[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
