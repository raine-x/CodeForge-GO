package server

import (
	"strings"
	"testing"

	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

// 上下文进度条的「口径」测试。
//
// 进度条的意义只在一点：数字必须和自动压缩用同一把尺子，否则 100% 就不是
// 「即将压缩」的阈值，用户会误判。所以这里不对字符串做断言，只钉住
// used/raw == agent.EstimateTokens(...)、percent == used/budget，以及
// over_budget / compressed 两个状态位的区别。
//
// ⚠️ 两个状态位是**不同**的事，别再混为一谈（这里踩过一次）：
//   - OverBudget = Used 越过压缩线（本步送模会触发压缩）；
//   - Compressed = 会话里**已经存在**摘要（压缩发生过）。
// 刚超线的那一瞬间 OverBudget=true 而 Compressed 仍为 false。
//
// fillSession 往指定会话的缓存对象里塞一条纯文本消息。
// History.Get 返回的是缓存里的活指针（见 history.go），因此无需 Save 即可被
// contextUsage 观察到 —— 测试不必启动完整服务。
func fillSession(t *testing.T, deps *testDeps, runes int) string {
	t.Helper()
	metas := deps.agent.History().List("", false)
	if len(metas) == 0 {
		t.Fatal("测试依赖未创建会话")
	}
	sess, ok := deps.agent.History().Get(metas[0].ID)
	if !ok {
		t.Fatalf("会话不存在: %s", metas[0].ID)
	}
	sess.Messages = []llm.Message{llm.TextMessage(llm.RoleUser, strings.Repeat("绣", runes))}
	return sess.ID
}

// TestContextUsageEmptySession 空会话/不存在的会话必须给出可展示的全零结果，
// 而不是报错或省略字段（前端进度条据此显示空条 + 0.0%）。
func TestContextUsageEmptySession(t *testing.T) {
	srv := newTestDeps(t).newServer()

	got := srv.contextUsage("不存在的会话")
	if got["type"] != "context" {
		t.Errorf("type = %v，期望 context", got["type"])
	}
	for _, k := range []string{"used", "raw", "messages", "summarized"} {
		if got[k] != 0 {
			t.Errorf("%s = %v，空会话应为 0", k, got[k])
		}
	}
	if got["percent"] != 0.0 {
		t.Errorf("percent = %v，期望 0.0", got["percent"])
	}
	if got["compressed"] != false || got["over_budget"] != false {
		t.Errorf("空会话不应有压缩状态，实际 compressed=%v over_budget=%v",
			got["compressed"], got["over_budget"])
	}
	// 测试环境未接模型窗口，压缩线回退到 agent.context_token_budget（缺省 120000）。
	if got["budget"] != 120000 {
		t.Errorf("budget = %v，期望 120000（窗口未知时的兜底压缩线）", got["budget"])
	}
	if got["window"] != 0 {
		t.Errorf("window = %v，期望 0（测试环境未配置模型窗口）", got["window"])
	}
}

// TestContextUsageMatchesEstimator 钉住口径：used 必须等于 agent.EstimateTokens
// 对「送模视图」的估算，percent 必须是「千分比取整后的一位小数百分比」。
// 未发生压缩时送模视图就是完整历史，故 used == raw。
func TestContextUsageMatchesEstimator(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()
	id := fillSession(t, deps, 6000) // 6000 个 CJK 字 ≈ 6004 tokens

	sess, _ := deps.agent.History().Get(id)
	want := agent.EstimateTokens(sess.Messages)
	got := srv.contextUsage(id)

	if got["used"] != want {
		t.Errorf("used = %v，期望 %v（必须与上下文压缩同口径）", got["used"], want)
	}
	if got["raw"] != want {
		t.Errorf("raw = %v，期望 %v（未压缩时 raw == used）", got["raw"], want)
	}
	if got["messages"] != 1 {
		t.Errorf("messages = %v，期望 1", got["messages"])
	}
	budget, _ := got["budget"].(int)
	if p, _ := got["percent"].(float64); budget > 0 && p != float64(want*1000/budget)/10 {
		t.Errorf("percent = %v，期望 %v", got["percent"], float64(want*1000/budget)/10)
	}
}

// TestContextUsageFlagsOverBudget 越过压缩线时必须标记 over_budget，
// 且 percent 不封顶（前端把条画满、明细里照实显示 >100%）。
//
// 同时钉住「两个状态位不同」：这里只是历史太长、**还没压过**，
// 所以 over_budget=true 而 compressed 必须仍是 false。
func TestContextUsageFlagsOverBudget(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()
	id := fillSession(t, deps, 400000) // 40 万 CJK 字 ≈ 400004 tokens ≫ 120000

	got := srv.contextUsage(id)
	if got["over_budget"] != true {
		t.Errorf("越过压缩线应标记 over_budget，实际 %v（used=%v budget=%v）",
			got["over_budget"], got["used"], got["budget"])
	}
	if got["compressed"] != false {
		t.Errorf("尚未发生摘要压缩时 compressed 应为 false，实际 %v（summarized=%v）",
			got["compressed"], got["summarized"])
	}
	if got["summarized"] != 0 {
		t.Errorf("summarized = %v，期望 0（未被摘要覆盖任何消息）", got["summarized"])
	}
	if p, _ := got["percent"].(float64); p <= 100 {
		t.Errorf("percent = %v，超预算时应 >100 且不封顶", got["percent"])
	}
}
