package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/llm"
)

// 上下文压缩的回归测试。
//
// 这里钉住三件「坏了就很难查」的事：
//  1. 估算口径（CJK 不能低估，否则压缩不触发 → 请求超窗被上游拒）；
//  2. 压缩的纯函数性（历史绝不能被写穿 —— 旧实现 copy 浅拷贝导致
//     「发一次请求顺手删掉自己的历史」）；
//  3. 切点的 API 合法性（tool_use / tool_result 必须成对，
//     在两者之间切开会让 Anthropic 直接拒绝整批消息）。

// ctxStubProvider 是只用于上下文测试的假适配器：
// 记录收到的请求（便于断言摘要提示词），并按配置返回摘要或错误。
type ctxStubProvider struct {
	reply   string
	err     error
	prompts []string // 每次调用的 user 消息文本
}

func (p *ctxStubProvider) Name() string { return "ctx-stub" }

func (p *ctxStubProvider) Stream(_ context.Context, req llm.Request) (<-chan llm.StreamEvent, error) {
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockText {
				p.prompts = append(p.prompts, b.Text)
			}
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	ch := make(chan llm.StreamEvent, 2)
	go func() {
		defer close(ch)
		if p.reply != "" {
			ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: p.reply}
		}
		ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	}()
	return ch, nil
}

// newCtxAgent 直接构造 Agent（同包可写未导出字段），
// 绕过 executor/registry 等与压缩无关的依赖。
func newCtxAgent(cfg config.AgentConfig, llmCfg config.LLMConfig, p llm.Provider) *Agent {
	return &Agent{cfg: cfg, llmCfg: llmCfg, provider: p}
}

// buildTurns 构造 n 轮「提问 → 调工具 → 工具结果 → 回答」的历史，
// 末尾再追加一条当前提问。每段正文 bodyRunes 个汉字。
func buildTurns(n, bodyRunes int) []llm.Message {
	body := strings.Repeat("绣", bodyRunes)
	var msgs []llm.Message
	for i := 0; i < n; i++ {
		msgs = append(msgs, llm.TextMessage(llm.RoleUser, fmt.Sprintf("第%d个问题 %s", i, body)))
		msgs = append(msgs, llm.AssistantBlocksMessage([]llm.ContentBlock{{
			Type:  llm.BlockToolUse,
			ID:    fmt.Sprintf("t%d", i),
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"a.go"}`),
		}}))
		msgs = append(msgs, llm.ToolResultMessage(fmt.Sprintf("t%d", i), body, false))
		msgs = append(msgs, llm.TextMessage(llm.RoleAssistant, body))
	}
	return msgs
}

// assertToolPairing 校验消息序列满足模型 API 的硬约束。
func assertToolPairing(t *testing.T, msgs []llm.Message) {
	t.Helper()
	if len(msgs) > 0 && startsWithToolResult(msgs[0]) {
		t.Errorf("序列以 tool_result 开头，模型 API 会拒绝")
	}
	seenUse := map[string]bool{}
	for i, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse {
				seenUse[b.ID] = true
			}
			if b.Type == llm.BlockToolResult && !seenUse[b.ToolUseID] {
				t.Errorf("第 %d 条出现悬空 tool_result(%s)：其 tool_use 被切掉了", i, b.ToolUseID)
			}
		}
	}
	// 反向检查：被保留的 tool_use 必须有对应结果，否则 Anthropic 报
	// "tool_use ids were found without tool_result blocks"。
	seenResult := map[string]bool{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				seenResult[b.ToolUseID] = true
			}
		}
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolUse && !seenResult[b.ID] {
				t.Errorf("tool_use(%s) 没有对应的 tool_result", b.ID)
			}
		}
	}
}

// cloneForCompare 深拷贝消息序列，用于比对「调用后原文是否被改动」。
func cloneForCompare(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		c := make([]llm.ContentBlock, len(m.Content))
		copy(c, m.Content)
		out[i] = llm.Message{Role: m.Role, Content: c}
	}
	return out
}

// blockEqual 逐字段比较内容块（ContentBlock 含 json.RawMessage，不能用 ==）。
func blockEqual(a, b llm.ContentBlock) bool {
	return a.Type == b.Type && a.Text == b.Text && a.ID == b.ID &&
		a.Name == b.Name && string(a.Input) == string(b.Input) &&
		a.ToolUseID == b.ToolUseID && a.Content == b.Content && a.IsError == b.IsError &&
		a.MediaType == b.MediaType && a.Data == b.Data
}

// assertHistoryUnchanged 断言「压缩没有写穿会话历史」——
// 这是旧实现最严重的缺陷，必须逐块比对（不能只看条数）。
func assertHistoryUnchanged(t *testing.T, want, got []llm.Message) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("历史条数被改动：%d → %d", len(want), len(got))
	}
	for i := range got {
		if got[i].Role != want[i].Role || len(got[i].Content) != len(want[i].Content) {
			t.Fatalf("历史第 %d 条结构被改动 (%d 块 → %d 块)",
				i, len(want[i].Content), len(got[i].Content))
		}
		for j := range got[i].Content {
			if !blockEqual(got[i].Content[j], want[i].Content[j]) {
				t.Fatalf("历史第 %d 条第 %d 块被原地改写：%q → %q",
					i, j, want[i].Content[j].Content, got[i].Content[j].Content)
			}
		}
	}
}

// ---------- 1. 估算口径 ----------

// TestEstimateTokensCJKNotUnderestimated 中文不能按「字符数/3」低估：
// 6000 个汉字必须估到 6000 上下，而不是 2000。
func TestEstimateTokensCJKNotUnderestimated(t *testing.T) {
	n := 6000
	got := EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, strings.Repeat("绣", n))})
	if got < n {
		t.Errorf("6000 个汉字估成 %d tokens，低于 1 字 1 token 的保守下界（会漏触发压缩）", got)
	}
	if got > n*2 {
		t.Errorf("6000 个汉字估成 %d tokens，高估过头（会过早压缩）", got)
	}
}

// TestEstimateTokensASCIIFourPerToken 英文按 4 字符 ≈ 1 token。
func TestEstimateTokensASCIIFourPerToken(t *testing.T) {
	n := 4000
	got := EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleAssistant, strings.Repeat("a", n))})
	if got < n/4-2 || got > n/4+8 {
		t.Errorf("4000 个 ASCII 字符估成 %d tokens，期望约 %d", got, n/4)
	}
}

// TestEstimateTokensCountsToolBlocks 工具调用的参数与结果都要计入，
// 否则「读了一堆文件」的会话会被低估。
func TestEstimateTokensCountsToolBlocks(t *testing.T) {
	withTool := []llm.Message{
		llm.AssistantBlocksMessage([]llm.ContentBlock{{
			Type: llm.BlockToolUse, ID: "t1", Name: "read_file",
			Input: json.RawMessage(`{"path":"` + strings.Repeat("绣", 500) + `"}`),
		}}),
		llm.ToolResultMessage("t1", strings.Repeat("绣", 500), false),
	}
	got := EstimateTokens(withTool)
	if got < 1000 {
		t.Errorf("工具参数+结果共 1000 字，估成 %d tokens，明显偏低", got)
	}
}

// ---------- 2. Compress 的纯函数性 ----------

// TestCompressDoesNotMutateInput 回归「压缩写穿历史」的严重 bug：
// 旧实现 out := make(...); copy(out, msgs) 是浅拷贝，Content 切片共享底层
// 数组，于是替换占位符会原地改写 sess.Messages 并随 Save 落库 ——
// 用户会看到自己的历史凭空消失。Compress 必须是纯函数。
func TestCompressDoesNotMutateInput(t *testing.T) {
	msgs := buildTurns(20, 800) // 远超下面给的预算
	before := cloneForCompare(msgs)

	out := Compress(msgs, 3000)

	assertHistoryUnchanged(t, before, msgs)
	if len(out) == 0 {
		t.Fatal("压缩结果为空")
	}
}

// TestCompressReturnsSmallerView 机械压缩确实要能压下去（兜底路径有效）。
func TestCompressReturnsSmallerView(t *testing.T) {
	msgs := buildTurns(20, 800)
	out := Compress(msgs, 3000)
	if got := EstimateTokens(out); got > 3000 {
		t.Errorf("压缩后仍有 %d tokens，超过预算 3000", got)
	}
	assertToolPairing(t, out)
}

// TestCompressNoOpWithinBudget 未超预算时必须原样返回（不做无谓改动）。
func TestCompressNoOpWithinBudget(t *testing.T) {
	msgs := buildTurns(1, 10)
	out := Compress(msgs, 100000)
	if len(out) != len(msgs) {
		t.Errorf("未超预算却改动了消息数：%d → %d", len(out), len(msgs))
	}
}

// TestCompressKeepsTailOfLastUserMessage 回归：超长用户消息被机械压缩时，
// **末尾的诉求必须活下来**。
//
// 真实故障：用户粘贴 500KB 内容后追加一句「只回复数字1」，clipRunes 只保留
// 头部 2000 字符，于是模型收到一堆 x 前缀、完全看不到那句要求，只能回
// 「内容被截断了，没有读到需求」。诉求写在尾部是这个交互的常态（长日志、
// 报错栈后面跟一句要做什么），所以截断必须保留尾部。
func TestCompressKeepsTailOfLastUserMessage(t *testing.T) {
	const ask = "只回复数字1"
	// 头部是噪音，诉求在末尾 —— 与真实粘贴场景一致
	huge := strings.Repeat("x", 500000) + ask
	msgs := []llm.Message{llm.TextMessage(llm.RoleUser, huge)}

	out := Compress(msgs, 3000)
	if len(out) == 0 {
		t.Fatal("压缩后为空")
	}

	var got string
	for _, m := range out {
		for _, b := range m.Content {
			if b.Type == llm.BlockText {
				got = b.Text
			}
		}
	}
	if !strings.Contains(got, ask) {
		t.Fatalf("末尾诉求 %q 被截断丢失了 —— 这正是用户遇到的故障。压缩后尾部为：%q",
			ask, tailRunes(got, 40))
	}
	// 头部也应当保留（前缀里可能有必要的上下文）
	if !strings.HasPrefix(got, "x") {
		t.Errorf("头部也丢了，截断应保留首尾两端，实际开头：%q", headRunes(got, 20))
	}
	// 并且要真的压下去了，否则等于没修
	if n := len([]rune(got)); n >= 500000 {
		t.Errorf("压缩后仍有 %d 字符，未生效", n)
	}
	if !strings.Contains(got, "省略") {
		t.Errorf("缺少省略标记，用户无法判断内容被截断过：%q", headRunes(got, 60))
	}
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// ---------- 3. 切点的 API 合法性 ----------

// TestIsPlainUserText 只有「用户纯文本发言」才是安全切点。
func TestIsPlainUserText(t *testing.T) {
	cases := []struct {
		name string
		m    llm.Message
		want bool
	}{
		{"用户文本", llm.TextMessage(llm.RoleUser, "你好"), true},
		{"用户空文本", llm.TextMessage(llm.RoleUser, "   "), false},
		{"工具结果", llm.ToolResultMessage("t1", "结果", false), false},
		{"助手文本", llm.TextMessage(llm.RoleAssistant, "回答"), false},
		{
			"含工具调用", llm.AssistantBlocksMessage([]llm.ContentBlock{
				{Type: llm.BlockToolUse, ID: "t1", Name: "read_file"},
			}), false,
		},
	}
	for _, c := range cases {
		if got := isPlainUserText(c.m); got != c.want {
			t.Errorf("%s：isPlainUserText = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestChooseSplitNeverCutsToolPair 切点必须落在用户发言上 ——
// 这是「对话流历史正确」的核心：切开 tool_use/tool_result 会被 API 拒绝。
func TestChooseSplitNeverCutsToolPair(t *testing.T) {
	msgs := buildTurns(12, 300)

	for _, keepBudget := range []int{1, 200, 1000, 3000, 20000} {
		split := chooseSplit(msgs, keepBudget)
		if split <= 0 {
			t.Fatalf("keepBudget=%d：找不到切点", keepBudget)
		}
		if !isPlainUserText(msgs[split]) {
			t.Errorf("keepBudget=%d：切点 %d 不是用户纯文本发言", keepBudget, split)
		}
		assertToolPairing(t, msgs[split:])
	}
}

// TestChooseSplitFallsBackToLastUser 连当前一轮都超出保留预算时，
// 退到「最后一条用户发言」：提问必须留给模型，工具结果可以随摘要走。
func TestChooseSplitFallsBackToLastUser(t *testing.T) {
	msgs := buildTurns(8, 500)
	want := lastPlainUserIndex(msgs)
	if got := chooseSplit(msgs, 1); got != want {
		t.Errorf("保留预算极小时切点 = %d，期望退到最后一个用户发言 %d", got, want)
	}
}

// TestChooseSplitPrefersEarliestFit 预算充足时尽量多保留原文（摘要是有损的）。
func TestChooseSplitPrefersEarliestFit(t *testing.T) {
	msgs := buildTurns(10, 100)
	all := EstimateTokens(msgs)
	// 给一个能装下大半历史的保留预算：应当切在较早的用户发言上，
	// 而不是保守地只留最后一轮。
	got := chooseSplit(msgs, all*3/4)
	if got <= 0 {
		t.Fatal("找不到切点")
	}
	if got > lastPlainUserIndex(msgs) {
		t.Errorf("切点 %d 超过了最后一个用户发言", got)
	}
	if got >= len(msgs)-4 {
		t.Errorf("保留预算充足却只从 %d 切（共 %d 条），过于保守", got, len(msgs))
	}
}

// TestChooseSplitNoCandidate 整段历史无法切分时返回 0，让调用方走机械压缩。
func TestChooseSplitNoCandidate(t *testing.T) {
	msgs := []llm.Message{
		llm.ToolResultMessage("t1", strings.Repeat("结", 100), false),
		llm.TextMessage(llm.RoleAssistant, "回答"),
	}
	if got := chooseSplit(msgs, 10); got != 0 {
		t.Errorf("无可用切点时应返回 0，实际 %d", got)
	}
}

// ---------- 4. 摘要式压缩全链路 ----------

// TestPrepareMessagesSummarizes 超阈值时调用模型生成摘要，并且：
//   - 会话历史原文一条不少（用户仍能回看完整对话）；
//   - 送模视图 = 摘要 + 保留段，且显著变小；
//   - 产出 compress 事件供前端提示。
func TestRunLoopCompressesRequestOverhead(t *testing.T) {
	p := &ctxStubProvider{reply: "摘要"}
	a := newEmitTestAgent(t, p)
	messages := buildTurns(5, 200)
	messages = append(messages, llm.TextMessage(llm.RoleUser, "继续"))
	sess := &Session{ID: "overhead", Messages: messages}
	// 预算按当前真实的请求开销给：历史单独放得下（B > 历史），
	// 但扣掉「系统提示词 + 工具定义」的固定开销后放不下 —— 这正是要验证的口径。
	// 写成绝对数值的话，提示词或工具描述一加长就会撞上「开销已占满预算」的上限告警。
	overhead := requestOverhead(a.systemPrompt(""), a.registry.DefinitionsFor(nil))
	a.cfg.ContextTokenBudget = EstimateTokens(messages) + overhead/2
	var compressed bool
	err := a.runLoopWithPersistence(context.Background(), sess, func(ev Event) {
		compressed = compressed || ev.Type == EventCompress
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !compressed {
		t.Fatal("system prompt overhead did not trigger compression")
	}
}

func TestPrepareMessagesSummarizes(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 2000, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{reply: "## 已完成的改动\n- 读取了 a.go"}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	msgs := buildTurns(12, 400)
	msgs = append(msgs, llm.TextMessage(llm.RoleUser, "现在总结一下"))
	sess := &Session{ID: "s1", Messages: msgs}
	before := cloneForCompare(msgs)

	var events []Event
	view := a.prepareMessages(context.Background(), sess, func(e Event) { events = append(events, e) })

	// 历史必须完好无损。
	assertHistoryUnchanged(t, before, sess.Messages)

	// 压缩状态已推进，送模视图显著变小。
	if sess.compressedUpTo <= 0 {
		t.Fatal("compressedUpTo 未推进，压缩没生效")
	}
	if !strings.Contains(sess.summaryText, "已完成的改动") {
		t.Errorf("摘要未落入会话状态：%q", sess.summaryText)
	}
	if len(prov.prompts) == 0 {
		t.Fatal("没有调用模型生成摘要")
	}
	beforeTokens, afterTokens := EstimateTokens(before), EstimateTokens(view)
	if afterTokens >= beforeTokens {
		t.Errorf("送模视图没有变小：%d → %d tokens", beforeTokens, afterTokens)
	}
	if afterTokens > cfg.ContextTokenBudget {
		t.Errorf("压缩后仍超预算：%d > %d", afterTokens, cfg.ContextTokenBudget)
	}

	// 送模视图第一条是摘要，角色为 user，且序列合法。
	if len(view) == 0 || view[0].Role != llm.RoleUser {
		t.Fatalf("摘要消息缺失或角色不对：%+v", view[0])
	}
	assertToolPairing(t, view)

	// 事件。
	var compressEv *Event
	for i := range events {
		if events[i].Type == EventCompress {
			compressEv = &events[i]
		}
	}
	if compressEv == nil {
		t.Fatal("未发出 compress 事件，前端无从提示")
	}
	if compressEv.Compress == nil || compressEv.Compress.Degraded {
		t.Errorf("compress 事件内容异常：%+v", compressEv.Compress)
	}
	if compressEv.Compress.After >= compressEv.Compress.Before {
		t.Errorf("事件里的前后 token 数不合理：%+v", compressEv.Compress)
	}
}

// TestPrepareMessagesKeepsFullHistoryForReplay 反复压缩后历史依然完整：
// 老的 Messages 不会被摘要「吃掉」，这是与旧实现最本质的区别。
func TestPrepareMessagesKeepsFullHistoryForReplay(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 1500, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{reply: "摘要"}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	sess := &Session{ID: "s2", Messages: buildTurns(6, 300)}
	total := len(sess.Messages)

	// 模拟连续多轮：每轮追加提问与回答，然后压缩一次。
	for round := 0; round < 3; round++ {
		sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "继续，请再检查一遍"))
		sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleAssistant, strings.Repeat("好", 600)))
		a.prepareMessages(context.Background(), sess, nil)
	}

	if len(sess.Messages) != total+3*2 {
		t.Errorf("历史条数被压缩改动：期望 %d，实际 %d", total+3*2, len(sess.Messages))
	}
	if sess.compressedUpTo <= 0 || sess.compressedUpTo >= len(sess.Messages) {
		t.Errorf("压缩游标越界或不合理：%d（共 %d 条）", sess.compressedUpTo, len(sess.Messages))
	}
}

// TestPrepareMessagesIncrementalSummary 二次压缩必须把上一版摘要一起交给模型，
// 否则早期信息会随着「只摘要新增部分」而丢失。
func TestPrepareMessagesIncrementalSummary(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 1200, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{reply: "第一版摘要：用户在做绣球项目"}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	sess := &Session{ID: "s3", Messages: buildTurns(8, 300)}
	a.prepareMessages(context.Background(), sess, nil)
	first := sess.compressedUpTo
	if first <= 0 {
		t.Fatal("第一次压缩未生效")
	}

	// 追加更多历史再压一次。
	sess.Messages = append(sess.Messages, buildTurns(8, 300)...)
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "再总结"))
	prov.prompts = nil
	a.prepareMessages(context.Background(), sess, nil)

	if sess.compressedUpTo <= first {
		t.Errorf("第二次压缩没有推进游标：%d → %d", first, sess.compressedUpTo)
	}
	if len(prov.prompts) == 0 {
		t.Fatal("第二次压缩没有调用模型")
	}
	if !strings.Contains(prov.prompts[len(prov.prompts)-1], "第一版摘要：用户在做绣球项目") {
		t.Errorf("增量摘要没有带上旧摘要，早期信息会丢失：\n%s", prov.prompts[len(prov.prompts)-1])
	}
}

// TestSummarizeRangeChunksHugeHistory 历史远超窗口时分块滚动摘要，
// 绝不允许「为了摘要而超窗」。
func TestSummarizeRangeChunksHugeHistory(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 4000, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{reply: "累计摘要"}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	msgs := buildTurns(40, 600) // 远超 4000 token 的块预算
	summary, err := a.summarizeRange(context.Background(), "", msgs, 0, len(msgs))
	if err != nil {
		t.Fatalf("分块摘要失败：%v", err)
	}
	if summary == "" {
		t.Fatal("摘要为空")
	}
	if len(prov.prompts) < 2 {
		t.Errorf("预期分多块摘要，实际只调用 %d 次", len(prov.prompts))
	}
	// 每次请求的历史量都必须受块预算约束（留出提示词余量）。
	chunkCap := a.summarizeChunkBudget() * 2
	for i, p := range prov.prompts {
		est := EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, p)})
		if est > chunkCap {
			t.Errorf("第 %d 次摘要请求 %d tokens，超过块上限 %d（会超窗）", i, est, chunkCap)
		}
	}
}

// ---------- 5. 兜底与自愈 ----------

// TestPrepareMessagesFallsBackWhenProviderFails 摘要调用失败必须回退机械压缩：
// 宁可丢一部分工具结果，也不能让请求超窗报错。
func TestPrepareMessagesFallsBackWhenProviderFails(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 2000, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{err: fmt.Errorf("上游 503")}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	msgs := buildTurns(12, 400)
	msgs = append(msgs, llm.TextMessage(llm.RoleUser, "总结"))
	sess := &Session{ID: "s4", Messages: msgs}
	before := cloneForCompare(msgs)

	var events []Event
	view := a.prepareMessages(context.Background(), sess, func(e Event) { events = append(events, e) })

	if EstimateTokens(view) > cfg.ContextTokenBudget {
		t.Errorf("兜底后仍超预算：%d > %d", EstimateTokens(view), cfg.ContextTokenBudget)
	}
	assertToolPairing(t, view)

	// 回退是临时的：不得记成「已摘要」，否则下一轮不会再尝试摘要。
	if sess.compressedUpTo != 0 {
		t.Errorf("机械兜底不应推进 compressedUpTo，实际 %d", sess.compressedUpTo)
	}
	// 历史同样不能被写穿。
	assertHistoryUnchanged(t, before, sess.Messages)

	degraded := false
	for _, e := range events {
		if e.Type == EventCompress && e.Compress != nil && e.Compress.Degraded {
			degraded = true
		}
	}
	if !degraded {
		t.Error("回退机械压缩时未发出 degraded 事件，用户会以为摘要成功了")
	}
}

// TestPrepareMessagesNoProvider 没有适配器时也要能兜底，不能 panic。
func TestPrepareMessagesNoProvider(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 1500, ContextCompressRatio: 0.95}
	a := newCtxAgent(cfg, config.LLMConfig{}, nil)

	msgs := buildTurns(10, 400)
	msgs = append(msgs, llm.TextMessage(llm.RoleUser, "总结"))
	sess := &Session{ID: "s5", Messages: msgs}

	view := a.prepareMessages(context.Background(), sess, nil)
	if EstimateTokens(view) > cfg.ContextTokenBudget {
		t.Errorf("无适配器时未兜底：%d > %d", EstimateTokens(view), cfg.ContextTokenBudget)
	}
}

// TestCompressionSelfHealsAfterRegenerate Regenerate 会把历史截断到最后一条
// 用户消息，压缩游标可能越界，必须自愈，否则会切出不存在的区间。
func TestCompressionSelfHealsAfterRegenerate(t *testing.T) {
	sess := &Session{ID: "s6", Messages: buildTurns(10, 100)} // 40 条

	// 合法状态：压了前 20 条。
	sess.compressedUpTo = 20
	sess.summaryText = "旧摘要"
	if ok, n := sess.compressionState(); !ok || n != 20 {
		t.Errorf("合法状态被误判：(%v,%d)，期望 (true,20)", ok, n)
	}

	// 越界状态（游标超过总条数）必须判为未压缩。
	sess.compressedUpTo = 45
	if ok, n := sess.compressionState(); ok || n != 0 {
		t.Errorf("游标越界时 compressionState = (%v,%d)，期望 (false,0)", ok, n)
	}

	// 模拟 Regenerate 截断到最后一条用户消息，再自愈。
	sess.compressedUpTo = 20
	sess.summaryText = "旧摘要"
	sess.Messages = sess.Messages[:8]
	sess.normalizeCompression()
	if sess.compressedUpTo != 0 || sess.summaryText != "" {
		t.Errorf("截断后未复位压缩状态：upTo=%d summary=%q", sess.compressedUpTo, sess.summaryText)
	}
	// 复位后 requestView 必须退化为原始历史，而不是切出坏区间。
	a := &Agent{}
	if got := len(a.requestView(sess)); got != len(sess.Messages) {
		t.Errorf("requestView 未退化：%d vs %d", got, len(sess.Messages))
	}
}

// ---------- 6. 阈值口径 ----------

// TestCompressBudgetFromModelWindow 压缩线 = (模型窗口 − 输出预留) × 95%。
//
// 必须扣掉输出预留：一次请求的总量是「输入 + 输出」，若直接拿窗口的 95%
// 当输入预算，在「窗口 200k、输出上限 100k」这类配置下必然超窗。
func TestCompressBudgetFromModelWindow(t *testing.T) {
	// 期望值同样按 (窗口 − 预留) × 0.95 计算，避免在表里手写字面量。
	smallOut := 8192
	cases := []struct {
		name      string
		window    int
		maxTokens int
		budget    int
		reserve   int
	}{
		{"窗口已知·输出上限占一半", 200000, 100000, 95000, 100000},
		{"窗口已知·输出上限很小", 200000, smallOut, int(math.Round(float64(200000-smallOut) * 0.95)), smallOut},
		{"输出上限超过窗口一半·夹到一半", 200000, 400000, 95000, 100000},
		{"窗口未知·回退绝对阈值", 0, 8192, 120000, 0},
	}
	for _, c := range cases {
		a := newCtxAgent(
			config.AgentConfig{ContextTokenBudget: 120000, ContextCompressRatio: 0.95},
			config.LLMConfig{MaxTokens: c.maxTokens}, nil)
		a.SetContextWindow(c.window)

		if got := a.compressBudget(); got != c.budget {
			t.Errorf("%s：compressBudget = %d，期望 %d", c.name, got, c.budget)
		}
		if got := a.ContextReserve(); got != c.reserve {
			t.Errorf("%s：ContextReserve = %d，期望 %d", c.name, got, c.reserve)
		}
	}
}

// TestCompressRatioClamped 非法比例回退到 0.80，不让配置写坏阈值。
func TestCompressRatioClamped(t *testing.T) {
	for _, bad := range []float64{0, -1, 1, 2} {
		a := newCtxAgent(config.AgentConfig{ContextCompressRatio: bad}, config.LLMConfig{MaxTokens: 1000}, nil)
		a.SetContextWindow(100000)
		want := int(float64(100000-1000) * 0.80)
		if got := a.compressBudget(); got != want {
			t.Errorf("比例 %v 未被夹紧：budget = %d，期望 %d", bad, got, want)
		}
	}
}

// TestContextStatReportsSummaryUsage 进度条口径：压缩后 used 应小于 raw，
// 并如实报告被摘要的条数，让界面能解释「为什么历史没变但占用降了」。
func TestContextStatReportsSummaryUsage(t *testing.T) {
	cfg := config.AgentConfig{ContextTokenBudget: 2000, ContextCompressRatio: 0.95}
	prov := &ctxStubProvider{reply: "很短的摘要"}
	a := newCtxAgent(cfg, config.LLMConfig{}, prov)

	msgs := buildTurns(12, 400)
	msgs = append(msgs, llm.TextMessage(llm.RoleUser, "总结"))
	sess := &Session{ID: "s7", Messages: msgs}

	if st := a.ContextStat(sess); st.Compressed || st.Summarized != 0 {
		t.Errorf("压缩前不该标记 compressed：%+v", st)
	}

	a.prepareMessages(context.Background(), sess, nil)

	st := a.ContextStat(sess)
	if !st.Compressed || st.Summarized <= 0 {
		t.Errorf("压缩后应标记 compressed 并给出条数：%+v", st)
	}
	if st.Used >= st.Raw {
		t.Errorf("压缩后 used(%d) 应小于 raw(%d)", st.Used, st.Raw)
	}
	if st.Messages != len(msgs) {
		t.Errorf("Messages = %d，期望原始条数 %d（历史必须完整）", st.Messages, len(msgs))
	}
	if st.Used > st.Budget {
		t.Errorf("压缩后仍超预算：used=%d budget=%d", st.Used, st.Budget)
	}
}

// TestContextStatNilSession 无会话时返回可展示的全零结果（含有效预算），
// 前端进度条据此显示空条而不是报错。
func TestContextStatNilSession(t *testing.T) {
	a := newCtxAgent(config.AgentConfig{ContextTokenBudget: 12345}, config.LLMConfig{}, nil)
	st := a.ContextStat(nil)
	if st.Used != 0 || st.Messages != 0 || st.Compressed || st.OverBudget {
		t.Errorf("空会话应全零：%+v", st)
	}
	if st.Budget != 12345 {
		t.Errorf("Budget = %d，期望 12345", st.Budget)
	}
}

// TestConsumeStreamAccumulatesUsage 用量累计：LLM 流里的 EventUsage 必须落到
// 会话上，ContextStat 才能给出「已使用总 / 缓存命中 / 缓存未命中」。
// 未命中 = 输入总数 − 缓存命中（口径见 llm.Usage 注释）。
func TestConsumeStreamAccumulatesUsage(t *testing.T) {
	a := newCtxAgent(config.AgentConfig{}, config.LLMConfig{}, &ctxStubProvider{})
	sess := &Session{ID: "s-usage"}
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: &llm.Usage{
		InputTokens: 620, CachedTokens: 500, OutputTokens: 42,
	}}
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "回答"}
	close(ch)

	if _, err := a.consumeStream(context.Background(), sess, ch, func(Event) {}); err != nil {
		t.Fatalf("consumeStream 报错: %v", err)
	}
	st := a.ContextStat(sess)
	if st.TotalTokens != 662 {
		t.Errorf("TotalTokens = %d，期望 662（620 输入 + 42 输出）", st.TotalTokens)
	}
	if st.CacheHit != 500 {
		t.Errorf("CacheHit = %d，期望 500", st.CacheHit)
	}
	if st.CacheMiss != 120 {
		t.Errorf("CacheMiss = %d，期望 120（620 − 500）", st.CacheMiss)
	}
}
