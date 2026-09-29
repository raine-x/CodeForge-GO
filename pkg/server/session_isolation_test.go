package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 跨会话消息越界的回归护栏。
//
// 用户报告：同一项目里两个会话，在第二个会话发消息，消息却跑进了第一个会话，
// 顶部还出现「已把这条指令转向到后台正在运行的任务」。
//
// 根因两处叠加：
//
//  1. 服务端 load_session 被拒时发 error 帧，而前端 case 'error' 会**无条件**
//     `sessionChanging = false` + `dropOptimisticBubble()`。这一帧抢在随后的
//     history 帧之前到达，把切换状态机打断 → sessionID 永远不更新。
//     用户以为自己在会话 B，实际视图还停在 A。
//  2. 前端发消息时只要 `running` 为真就走 steerNow()，而 steerNow 的转向目标是
//     `runSessionID || sessionID` —— 后台那个会话。于是 B 里的字送进 A。
//
// ui.js 无构建步骤，测试只能读源码断言（web/test/render_md.test.js 已是这个做法）。
// 改成别的形态会红，这是刻意的：这几条都是「形状」约束，不是「行为」约束。

func uiSource(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "web", "dist", "ui.js")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 ui.js 失败: %v", err)
	}
	return string(b)
}

func wsSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ws_handler.go")
	if err != nil {
		t.Fatalf("读取 ws_handler.go 失败: %v", err)
	}
	return string(b)
}

// braceBlock 按大括号配平截取从 openIdx 起的第一个 {...}。
func braceBlock(t *testing.T, src string, from int) string {
	t.Helper()
	open := strings.Index(src[from:], "{")
	if open < 0 {
		t.Fatalf("从偏移 %d 起找不到 '{'", from)
	}
	depth := 0
	for i := from + open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[from+open : i+1]
			}
		}
	}
	t.Fatal("大括号未配平")
	return ""
}

// funcBody 截取具名函数的函数体（含大括号）。
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	idx := strings.Index(src, "function "+name)
	if idx < 0 {
		t.Fatalf("ui.js 里找不到函数 %s", name)
	}
	return braceBlock(t, src, idx)
}

// TestSteerNeverTargetsAnotherSession 转向绝不跨会话。
//
// 这是用户报告的直接症状：消息跑进另一个会话 +「已把这条指令转向到
// 后台正在运行的任务」。
//
// 契约：steerNow 只在「**当前视图这个会话**正在跑」时生效；否则返回 false，
// 由调用方按新消息处理（不吞消息）。
func TestSteerNeverTargetsAnotherSession(t *testing.T) {
	body := funcBody(t, uiSource(t), "steerNow")

	if regexp.MustCompile(`runSessionID\s*\|\|\s*sessionID`).MatchString(body) {
		t.Errorf("steerNow 仍以 runSessionID 作为转向目标 —— "+
			"视图在会话 B、后台跑着 A 时，用户在 B 里打的字会被送进 A。\n%s", body)
	}
	// 必须用当前会话的 sessionID，且不能拿 runSessionID 兜底。
	if !strings.Contains(body, "const target = sessionID") {
		t.Errorf("steerNow 的目标必须是当前会话的 sessionID（不能是 runSessionID）\n%s", body)
	}
	// 必须判 viewRunning()：别的会话在跑时不能转向。
	// （原判据是 runAway()，那个单槽代理已于 2026-09 随「并发会话视图隔离」删除。）
	if !strings.Contains(body, "viewRunning()") {
		t.Errorf("steerNow 必须检查 viewRunning()（当前视图这个会话是否在跑）\n%s", body)
	}
	// 拒绝时返回 false，让调用方继续按新消息发 —— 返回 true 会把消息吞掉。
	if !strings.Contains(body, "if (!viewRunning()) return false") {
		t.Errorf("当前会话没在跑时 steerNow 必须 `return false`（让调用方按新消息处理）。\n"+
			"返回 true 会让这条消息既没发出去、也没留在输入框。\n%s", body)
	}
}

// TestSendDoesNotSteerWhenAnotherSessionRunning 点发送时，
// 别的会话在跑不能让当前会话的消息走转向分支。
func TestSendDoesNotSteerWhenAnotherSessionRunning(t *testing.T) {
	src := uiSource(t)
	// 判据是 viewRunning()（「**当前视图这个会话**在跑」），不是 anyRunning()。
	// 两者混用就会把「别的会话在后台跑」当成「本会话在跑」，当前会话的新消息
	// 被 steerNow 改道成转向。
	if !strings.Contains(src, "if (viewRunning()) {") {
		t.Fatalf("发消息流程缺少 `if (viewRunning())` 判据 —— " +
			"少了它，别的会话在跑时当前会话的消息会被 steerNow 改道")
	}
	// 有字时：steerNow 返回 false 要**继续往下走**（正常发送），
	// 不能无条件 return 把消息吞掉。
	// ⚠️ 必须锚在 submitMessage 里那处，不能取全文件第一个 `if (viewRunning())`：
	// syncSendBtn 里也有同形的判断，取错了会截到按钮提示那段，断言必然假失败。
	const anchor = "async function submitMessage("
	anchorAt := strings.Index(src, anchor)
	if anchorAt < 0 {
		t.Fatal("ui.js 里找不到 submitMessage")
	}
	// ⚠️ 下标要加回 anchorAt：strings.Index 返回的是**子串内**的偏移，
	// 直接拿去切 src 会落到文件前部，截出一段无关代码。
	seg := src[anchorAt+strings.Index(src[anchorAt:], "if (viewRunning()) {"):]
	end := strings.Index(seg, "\n    }")
	if end < 0 || end > 800 {
		end = minInt(800, len(seg))
	}
	block := seg[:end]
	if strings.Contains(block, "if (String(input.value).trim()) { steerNow(); return; }") {
		t.Errorf("转向分支必须看 steerNow 的返回值：`if (steerNow()) return;`。"+
			"无条件 return 会在 steerNow 拒绝时把消息吞掉。\n%s", block)
	}
	// 打断必须带 session_id，否则服务端不知道停哪个会话。
	if !strings.Contains(block, "type: 'cancel', session_id: sessionID") {
		t.Errorf("打断必须带 session_id —— 不带的话服务端无法只停当前会话\n%s", block)
	}
}

// TestEscapeDoesNotCancelAnotherSession Esc 也不该打断别的会话。
func TestEscapeDoesNotCancelAnotherSession(t *testing.T) {
	src := uiSource(t)
	idx := strings.Index(src, "if (e.key !== 'Escape') return;")
	if idx < 0 {
		t.Skip("ui.js 里找不到 Esc 处理分支")
	}
	seg := src[idx:minInt(idx+1200, len(src))]

	cancelIdx := strings.Index(seg, "type: 'cancel'")
	if cancelIdx < 0 {
		t.Fatal("Esc 分支里找不到 cancel")
	}
	if !strings.Contains(seg[:cancelIdx], "viewRunning()") {
		t.Errorf("Esc 打断前必须先判 viewRunning() —— 否则在会话 B 按 Esc 会停掉后台的 A。\n%s",
			seg[:cancelIdx])
	}
	if !strings.Contains(seg, "type: 'cancel', session_id: sessionID") {
		t.Errorf("Esc 的 cancel 必须带 session_id")
	}
}

// TestBusyIdleCarrySessionID busy/idle 必须带会话号。
//
// 并行跑多个会话时，前端靠 ev.session_id 判断「这帧属于哪个会话」。
// 缺了它就会用当前视图的 sessionID 顶替 —— 那正是最初串会话的起点。
//
// 例外：cancel 不带 session_id 时的「无任务可停」兜底 idle 允许缺 ——
// 那条路径没有任何会话上下文可带，前端也不会拿它判归属。
func TestBusyIdleCarrySessionID(t *testing.T) {
	b, err := os.ReadFile("ws_handler.go")
	if err != nil {
		t.Fatalf("读取 ws_handler.go 失败: %v", err)
	}
	src := string(b)

	// 逐个找所有 idle 帧，逐条判定：带 reason:"cancelled" 且无 session_id 的
	// 只能是「cancel 没带 id 且无事可停」那条兜底。
	l := strings.Index(src, `"type": "idle"`)
	for l != -1 {
		line := src[l:minInt(l+150, len(src))]
		// 截到该 map 字面量结束
		if end := strings.Index(line, "}"); end > 0 {
			line = line[:end]
		}
		if !strings.Contains(line, "session_id") {
			isCancelFallback := strings.Contains(line, `"cancelled"`) &&
				!strings.Contains(line, "msg.SessionID")
			if !isCancelFallback {
				t.Errorf("idle 帧必须带 session_id（并行多会话时前端靠它区分归属）。\n实际：%s", line)
			}
		}
		n := strings.Index(src[l+1:], `"type": "idle"`)
		if n < 0 {
			break
		}
		l = l + 1 + n
	}

	for _, kind := range []string{"busy"} {
		pat := `"type": "` + kind + `"`
		idx := strings.Index(src, pat)
		if idx < 0 {
			t.Errorf("ws_handler.go 里找不到 %s 帧", kind)
			continue
		}
		line := src[idx:minInt(idx+120, len(src))]
		if !strings.Contains(line, "session_id") {
			t.Errorf("%s 帧必须带 session_id —— 并行多会话时前端靠它区分归属。实际：%s", kind, line)
		}
	}
}

// TestServerRunTableIsPerSession 服务端的运行表必须按会话分槽。
//
// 曾经是单个 cancel 槽 + c.stop()，于是：
//   - 在 B 会话发消息会先把 A 正在跑的那轮杀掉
//   - 同一连接上无法并行跑两个会话
func TestServerRunTableIsPerSession(t *testing.T) {
	b, err := os.ReadFile("ws_handler.go")
	if err != nil {
		t.Fatalf("读取 ws_handler.go 失败: %v", err)
	}
	src := string(b)

	if line, ok := firstCodeLineWith(src, "c.stop()"); ok {
		t.Errorf("仍有真实代码调用 c.stop()（不分会话的打断）\n%s", line)
	}
	if !strings.Contains(src, "runs   map[string]*sessionRun") &&
		!strings.Contains(src, "runs map[string]*sessionRun") {
		t.Errorf("wsClient 缺少按会话索引的运行表 runs map[string]*sessionRun")
	}
	if !strings.Contains(src, "func (c *wsClient) stopSession(sessionID string) bool") {
		t.Errorf("缺少按会话打断的 stopSession")
	}
}

// firstCodeLineWith 找第一处**不在注释里**的 sub。
//
// 必须排除注释：这个文件里到处是「曾经这里是 c.stop()」这类说明，
// 它们记录了历史，但显然不该被判成违规调用。
func firstCodeLineWith(src, sub string) (string, bool) {
	for i := 0; i+len(sub) <= len(src); i++ {
		if src[i:i+len(sub)] != sub {
			continue
		}
		lineStart := strings.LastIndex(src[:i], "\n") + 1
		lineEnd := strings.Index(src[i:], "\n")
		if lineEnd < 0 {
			lineEnd = len(src) - i
		}
		line := src[lineStart : i+lineEnd]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		return line, true
	}
	return "", false
}

// TestLoadSessionBlockedDoesNotUseErrorFrame 服务端切换受阻不许发 error 帧。
func TestLoadSessionBlockedDoesNotUseErrorFrame(t *testing.T) {
	src := wsSource(t)
	idx := strings.Index(src, `case "load_session":`)
	if idx < 0 {
		t.Fatal("ws_handler.go 里找不到 load_session 分支")
	}
	seg := src[idx:]
	if end := strings.Index(seg[1:], "\n\tcase "); end > 0 {
		seg = seg[:end+1]
	}

	// 「会话不存在」那一条 error 保留 —— 那是真错误，且此时尚未开始切换。
	// 被禁的是「切换受阻」路径上的 error。
	usesSendErr := strings.Contains(seg, "sendErr(")
	rawErrorFrames := strings.Count(seg, `"type": "error"`)
	notFound := strings.Count(seg, "会话不存在")

	if usesSendErr || rawErrorFrames > notFound {
		t.Errorf("load_session 的切换受阻路径用了 error 帧。\n"+
			"前端 case 'error` 会无条件 `sessionChanging = false` 与 "+
			"`dropOptimisticBubble()`，且它抢在随后的 history 帧之前到达 —— "+
			"切换被打断、sessionID 不更新，用户在 B 会话发的消息会落进 A。\n"+
			"应改用独立的 workspace_blocked 帧（只提示，不碰切换状态）。\n\n%s", seg)
	}
	if !strings.Contains(seg, "workspace_blocked") {
		t.Errorf("load_session 被拒时应发 workspace_blocked 帧而不是 error 帧。\n\n%s", seg)
	}
}

// TestFrontendHandlesWorkspaceBlocked 前端认这个帧，且只提示不动状态。
func TestFrontendHandlesWorkspaceBlocked(t *testing.T) {
	src := uiSource(t)
	idx := strings.Index(src, "case 'workspace_blocked'")
	if idx < 0 {
		t.Fatal("ui.js 没有处理 workspace_blocked 帧 —— 服务端发了没人收")
	}
	block := braceBlock(t, src, idx)

	for _, forbidden := range []string{
		"sessionChanging = false",
		"dropOptimisticBubble",
		"clearRunVisuals",
		"replayHistory",
		"sessionID =",
		"runSessionID =",
		"runningSessions.",
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("workspace_blocked 分支里出现了 %q —— 它必须只做提示。\n"+
				"碰切换状态会重演「消息落进另一个会话」。\n%s", forbidden, block)
		}
	}
	if !strings.Contains(block, "addInfo") {
		t.Errorf("workspace_blocked 分支必须用 addInfo 提示用户\n%s", block)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
