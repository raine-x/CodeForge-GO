// Package agent 实现 Agent 核心引擎：ReAct 循环、上下文管理与会话历史。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// 事件类型（推送给前端）。
const (
	EventUser        = "user"
	EventStep        = "step"
	EventText        = "text"
	EventToolCall    = "tool_call"
	EventToolResult  = "tool_result"
	EventHitlRequest = "hitl_request"
	EventRetry       = "retry"    // 上游瞬时故障自动重试中
	EventCompress    = "compress" // 上下文超阈值，已自动摘要压缩
	EventDone        = "done"
	EventError       = "error"
)

// Event 是 Agent 推送给前端的统一事件。
type Event struct {
	Type        string            `json:"type"`
	Step        int               `json:"step,omitempty"`
	Text        string            `json:"text,omitempty"`
	ToolCallID  string            `json:"tool_call_id,omitempty"`
	ToolName    string            `json:"tool_name,omitempty"`
	ToolInput   json.RawMessage   `json:"tool_input,omitempty"`
	Decision    string            `json:"decision,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	Result      *tools.ToolResult `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	Attempt     int               `json:"attempt,omitempty"`      // 重试类事件：即将进行的第几次尝试（1-based）
	MaxAttempts int               `json:"max_attempts,omitempty"` // 重试类事件：含首次请求在内的总尝试次数
	DiffStats   *DiffStats        `json:"diff_stats,omitempty"`   // 编辑类工具的 +/- 行数（供前端绿增红减展示）
	Compress    *CompressInfo     `json:"compress,omitempty"`     // 上下文压缩明细（仅 EventCompress 携带）
}

// DiffStats 是一次文件编辑的行数统计。
type DiffStats struct {
	Added   int `json:"added"`   // 新增行数（+）
	Removed int `json:"removed"` // 删除行数（-）
}

// CompressInfo 描述一次自动上下文压缩的结果。
type CompressInfo struct {
	Summarized  int    `json:"summarized"`       // 累计被摘要覆盖的消息条数
	Added       int    `json:"added"`            // 本次新并入摘要的条数
	Before      int    `json:"before"`           // 压缩前「实际送模」估算 tokens
	After       int    `json:"after"`            // 压缩后「实际送模」估算 tokens
	Budget      int    `json:"budget"`           // 触发压缩的预算（阈值）
	Window      int    `json:"window"`           // 模型上下文窗口（0 = 未配置）
	Incremental bool   `json:"incremental"`      // 是否在既有摘要基础上增量合并
	Degraded    bool   `json:"degraded"`         // 摘要不可用，已回退机械压缩
	Reason      string `json:"reason,omitempty"` // 回退原因（Degraded 时）
}

// diffStatsFor 对支持 PreviewDiff 的工具调用统计 +/- 行数。
// 返回 nil 表示该工具或该次调用不产生 diff。
func (a *Agent) diffStatsFor(tc llm.ToolCall) *DiffStats {
	tool, ok := a.registry.Get(tc.Name)
	if !ok {
		return nil
	}
	dp, ok := tool.(tools.DiffProvider)
	if !ok {
		return nil
	}
	diff, err := dp.PreviewDiff(tc.Input)
	if err != nil || diff == "" {
		return nil
	}
	st := &DiffStats{}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			st.Added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			st.Removed++
		}
	}
	return st
}

// Emitter 是事件发送回调。
type Emitter func(Event)

// Agent 是 ReAct 主循环引擎。
type Agent struct {
	cfg           config.AgentConfig
	provider      llm.Provider
	executor      *tools.Executor
	registry      *tools.Registry
	history       *History
	workDir       string
	llmCfg        config.LLMConfig // 请求参数（MaxTokens/Temperature 等），热切换后更新
	memoryStore   *store.Store     // 用户记忆存储（与 History 共用同一 SQLite 库）
	lastUserInput string           // 本轮用户输入（技能触发词匹配用）
	builtinOn     map[string]bool  // 内置插件启用表（key = 插件 ID）

	// 子智能体运行策略（并发上限 / 能力限制），设置页可热更新；
	// nil 表示使用 DefaultSubagentPolicy()。用锁保护是因为它会在
	// Agent 运行期间被 HTTP 处理器改写，而子智能体调度是并发的。
	subMu          sync.RWMutex
	subagentPolicy *SubagentPolicy

	// contextWindow 是当前生效模型的输入上下文窗口（tokens，来自模型库的
	// CtxIn 字段），0 表示未知（此时压缩线回退 agent.context_token_budget）。
	// 由 HTTP 处理器在模型应用时改写、由运行中的循环读取，故用原子量。
	contextWindow atomic.Int64

	// exposure 决定「每一轮 LLM 能看到哪些工具」（工具可见面过滤）。
	// nil = 暴露全部（等价于不设置）。只影响 Tools 定义下发，不改变
	// 工具的注册、执行、权限与审计链路。用锁保护因为它可能在 Agent
	// 运行期间被配置热更新改写，而 runLoop 是并发的。
	// 安全边界：Exposure 只回答「模型能不能收到这个工具的定义」（收不到
	// 自然就不会去调用）；「模型实际上能执行什么」由 Executor 的 Policy/
	// HITL 判定 —— 隐藏 ≠ 禁止执行，若需隐藏即禁用应在 Executor 层加规则。
	exposeMu sync.RWMutex
	exposure func(name string) bool
}

// New 构造 Agent 引擎。llmCfg 提供请求级参数（MaxTokens/Temperature），
// 与 AgentConfig（循环行为）分开传：前者随「应用/保存」模型热切换更新。
func New(cfg config.AgentConfig, llmCfg config.LLMConfig, provider llm.Provider, executor *tools.Executor, history *History, workDir string) *Agent {
	return &Agent{
		cfg:      cfg,
		llmCfg:   llmCfg,
		provider: provider,
		executor: executor,
		registry: executor.Registry(),
		history:  history,
		workDir:  workDir,
	}
}

// SetLLMConfig 热更新请求参数（MaxTokens/Temperature 等，随模型应用/保存切换）。
func (a *Agent) SetLLMConfig(c config.LLMConfig) { a.llmCfg = c }

// History 返回会话历史管理器。
func (a *Agent) History() *History { return a.history }

// SetProvider 热替换 LLM 适配器（用于前端修改配置后即时生效）。
func (a *Agent) SetProvider(p llm.Provider) { a.provider = p }

// SetWorkDir 热切换工作目录（用于前端切换工作区后即时生效）。
func (a *Agent) SetWorkDir(dir string) { a.workDir = dir }

// SetSubagentPolicy 热更新子智能体运行策略（设置 → 子智能体改动即时生效）。
// 策略会在下一个子智能体被创建 / 下一次委派校验时生效。
func (a *Agent) SetSubagentPolicy(p SubagentPolicy) {
	p = p.Normalize()
	a.subMu.Lock()
	a.subagentPolicy = &p
	a.subMu.Unlock()
}

// SubagentPolicy 返回当前子智能体运行策略（未设置时返回默认策略）。
func (a *Agent) SubagentPolicy() SubagentPolicy {
	a.subMu.RLock()
	defer a.subMu.RUnlock()
	if a.subagentPolicy == nil {
		return DefaultSubagentPolicy()
	}
	return *a.subagentPolicy
}

// WorkDir 返回当前工作目录（会话/记忆的 workspace 隔离键）。
func (a *Agent) WorkDir() string { return a.workDir }

// SetExposure 设定工具可见面过滤：pick 返回 true 的工具会被下发给 LLM，
// false 的工具隐藏（模型收不到定义，通常也不会去调用）。nil 恢复为暴露全部。
// 注意：隐藏只影响「可见面」，不等于禁止执行 —— 执行放行由 Executor/Policy
// 判定。只改变每轮 Tools 定义，不改变注册/执行/权限/审计链路。
func (a *Agent) SetExposure(pick func(name string) bool) {
	a.exposeMu.Lock()
	a.exposure = pick
	a.exposeMu.Unlock()
}

// exposureFn 返回当前工具可见面过滤（nil = 暴露全部）。返回的函数引用
// 在多次调用间可能被 SetExposure 替换，属预期：每次 runLoop 取一次。
// 只读路径直接内联函数值（无 CLOSURE 开销），不需要复制接口的全部字段。
func (a *Agent) exposureFn() func(name string) bool {
	a.exposeMu.RLock()
	defer a.exposeMu.RUnlock()
	return a.exposure
}

// SetContextWindow 设置当前生效模型的输入上下文窗口（tokens，来自模型库条目
// 的 ctx_in 字段）。传 0 表示未知，压缩线回退 agent.context_token_budget。
// 模型热切换（设置页「应用」）后由服务端同步，压缩阈值随之即时生效。
func (a *Agent) SetContextWindow(n int) {
	if n < 0 {
		n = 0
	}
	a.contextWindow.Store(int64(n))
}

// ContextWindow 返回当前生效模型的上下文窗口（0 = 未知）。
func (a *Agent) ContextWindow() int { return int(a.contextWindow.Load()) }

// inputWindow 返回「可供输入使用的窗口」（tokens）。
//
// 必须先从模型窗口里扣掉输出预留：一次请求的总量是「输入 + 输出」，
// 而压缩线只管住输入。若按窗口的 95% 直接当输入预算，在
// 「窗口 262144、输出上限 131072」这类配置下会算出 249k 输入，
// 加上输出必然超窗 —— 压缩反而变成了超窗的帮凶。
//
// 预留 = min(输出上限, 窗口/2)：至少保证输入占一半窗口，
// 避免输出上限设得比窗口还大时把输入挤成 0。
func (a *Agent) inputWindow() int {
	w := a.ContextWindow()
	if w <= 0 {
		return 0
	}
	reserve := a.llmCfg.MaxTokens
	if reserve <= 0 {
		reserve = summaryFallbackMaxTokens
	}
	if limit := w / 2; reserve > limit {
		reserve = limit
	}
	return w - reserve
}

// ContextReserve 返回为「输出」预留的 token 数（0 表示窗口未知）。
// 供前端明细解释「压缩线为什么小于模型窗口」。
func (a *Agent) ContextReserve() int {
	w := a.ContextWindow()
	if w <= 0 {
		return 0
	}
	return w - a.inputWindow()
}

// compressBudget 返回上下文压缩的触发线（tokens）。
//
// 优先按模型窗口算：(窗口 − 输出预留) × context_compress_ratio（缺省 0.95），
// 即「可用输入空间的占用达到 95% 就自动摘要压缩」。
// 模型窗口未知时回退到 agent.context_token_budget（缺省 120000），
// 保证任何配置下都存在一条有效的压缩线。
func (a *Agent) compressBudget() int {
	if iw := a.inputWindow(); iw > 0 {
		r := a.cfg.ContextCompressRatio
		if r < minCompressRatio || r > maxCompressRatio {
			r = defaultCompressRatio
		}
		// 四舍五入而非截断：0.95 在 float64 下是 0.9499999...，
		// 直接截断会算出 170999 这种差一 token 的别扭数字。
		return int(math.Round(float64(iw) * r))
	}
	if a.cfg.ContextTokenBudget > 0 {
		return a.cfg.ContextTokenBudget
	}
	return defaultContextBudget
}

// ContextStat 是一次「上下文占用」快照。
//
// 这是进度条与压缩判定共用的唯一口径：Used/Budget 就是压缩的输入，
// 因此进度条到 100% 的含义精确等于「下次请求前会触发压缩」，不会骗人。
type ContextStat struct {
	Used       int  // 实际送模的估算 tokens（已被摘要替换掉的部分不再计入）
	Raw        int  // 原始历史的估算 tokens（未压缩口径，用于对比展示）
	Budget     int  // 压缩触发线
	Window     int  // 模型上下文窗口（0 = 未配置）
	Reserve    int  // 输出预留（压缩线小于窗口的原因）
	Messages   int  // 原始历史消息条数
	Summarized int  // 已被摘要覆盖的消息条数
	Compressed bool // 是否处于压缩态（存在摘要）
	OverBudget bool // Used 是否已越过压缩线

	// ---- 用量统计（来自 LLM 上游的真实计费口径，进程内存态、重启归零）----
	TotalTokens int // 累计消耗 tokens（输入 + 输出）
	CacheHit    int // 累计命中上游提示缓存的输入 tokens
	CacheMiss   int // 累计未命中缓存的输入 tokens
}

// ContextStat 汇总指定会话的上下文占用。
func (a *Agent) ContextStat(sess *Session) ContextStat {
	budget := a.compressBudget()
	st := ContextStat{
		Budget:  budget,
		Window:  a.ContextWindow(),
		Reserve: a.ContextReserve(),
	}
	if sess == nil {
		return st
	}
	st.Raw = EstimateTokens(sess.Messages)
	st.Messages = len(sess.Messages)
	st.Compressed, st.Summarized = sess.compressionState()
	st.Used = EstimateTokens(a.requestView(sess))
	st.OverBudget = st.Used > budget
	st.TotalTokens = sess.usageIn + sess.usageOut
	st.CacheHit = sess.usageHit
	if miss := sess.usageIn - sess.usageHit; miss > 0 {
		st.CacheMiss = miss
	}
	return st
}

// prepareMessages 构造本步真正送给模型的消息序列，必要时先做摘要压缩。
//
// 与历史版本的关键区别：**sess.Messages 永远是完整、不可变的历史**，
// 压缩只影响「送模视图」。这样用户回看、Regenerate、界面回放都拿到完整对话，
// 不会再出现「发一次请求就少一段历史」。
func (a *Agent) prepareMessages(ctx context.Context, sess *Session, emit Emitter) []llm.Message {
	budget := a.compressBudget()

	// 会话历史可能被回退（Regenerate 截断）或被整体替换，先自愈压缩状态。
	sess.normalizeCompression()

	view := a.requestView(sess)
	used := EstimateTokens(view)
	if used <= budget {
		return view
	}

	// 保留段预算：留一半给摘要与后续几轮，避免刚压完立刻又触发。
	keepBudget := budget * keepRatioNum / keepRatioDen
	split := chooseSplit(sess.Messages, keepBudget)
	if split <= sess.compressedUpTo {
		// 没有可用的前进空间（整段历史是一个无法切分的巨轮，或切点已经压过）。
		return a.degradedCompress(sess, view, used, budget, split, emit)
	}

	prev := sess.summaryText
	added := split - sess.compressedUpTo
	summary, err := a.summarizeRange(ctx, prev, sess.Messages, sess.compressedUpTo, split)
	if err != nil || strings.TrimSpace(summary) == "" {
		log.Printf("[compress] 会话=%s 摘要压缩失败（已摘要 %d→%d 条）：%v",
			sess.ID, sess.compressedUpTo, split, err)
		return a.degradedCompress(sess, view, used, budget, split, emit)
	}

	sess.summaryText = summary
	sess.compressedUpTo = split

	next := a.requestView(sess)
	after := EstimateTokens(next)
	log.Printf("[compress] 会话=%s 阈值=%d 摘要累计 %d 条（本次新增 %d）%d → %d tokens",
		sess.ID, budget, split, added, used, after)

	if emit != nil {
		emit(Event{Type: EventCompress, Compress: &CompressInfo{
			Summarized:  split,
			Added:       added,
			Before:      used,
			After:       after,
			Budget:      budget,
			Window:      a.ContextWindow(),
			Incremental: strings.TrimSpace(prev) != "",
		}})
	}
	return next
}

// degradedCompress 是压缩的兜底路径：摘要不可用（调用失败 / 无安全切点 /
// 适配器未就绪）时退回机械压缩，保证请求一定不超窗。
//
// 刻意**不推进 compressedUpTo**：机械压缩是有损的临时手段，不应当被记成
// 「已摘要」。下一轮只要摘要恢复可用，仍会正常走摘要路径。
func (a *Agent) degradedCompress(sess *Session, view []llm.Message, used, budget, split int, emit Emitter) []llm.Message {
	out := Compress(view, budget)
	after := EstimateTokens(out)
	log.Printf("[compress] 会话=%s 摘要不可用，回退机械压缩（阈值=%d 候选切点=%d）%d → %d tokens",
		sess.ID, budget, split, used, after)
	if emit != nil {
		emit(Event{Type: EventCompress, Compress: &CompressInfo{
			Summarized: sess.compressedUpTo,
			Before:     used,
			After:      after,
			Budget:     budget,
			Window:     a.ContextWindow(),
			Degraded:   true,
			Reason:     "摘要不可用，已回退机械压缩",
		}})
	}
	return out
}

// requestView 返回「实际送模」的消息视图：
// 已压缩时 = 摘要消息 + 保留段原文；未压缩时 = 原始历史。
//
// 未压缩分支直接返回 sess.Messages（共享底层数组），这是安全的：
// 唯一的消费者是 llm 适配器，它只读；所有压缩路径都产出新切片。
func (a *Agent) requestView(sess *Session) []llm.Message {
	if sess.compressedUpTo <= 0 || strings.TrimSpace(sess.summaryText) == "" {
		return sess.Messages
	}
	tail := sess.Messages[sess.compressedUpTo:]
	out := make([]llm.Message, 0, len(tail)+1)
	out = append(out, summaryMessage(sess.summaryText))
	out = append(out, tail...)
	return out
}

// systemPrompt 返回本轮使用的 System Prompt。
// 在基础提示词之后动态拼装：用户记忆 / 项目说明（AGENTS.md、CLAUDE.md）/
// 技能索引与命中内容 —— 均按当前工作区隔离，无内容则整段省略。
func (a *Agent) systemPrompt() string {
	base := LoadSystemPrompt(a.cfg.SystemPromptFile, a.workDir)

	var extras []string
	if sec := MemorySection(a.loadMemories()); sec != "" {
		extras = append(extras, sec)
	}
	if sec := a.projectMemorySection(); sec != "" {
		extras = append(extras, sec)
	}
	if sec := a.builtinPluginSection(); sec != "" {
		extras = append(extras, sec)
	}
	if secs := a.skillSections(a.lastUserInput); len(secs) > 0 {
		extras = append(extras, secs...)
	}
	if len(extras) == 0 {
		return base
	}
	return base + "\n\n" + strings.Join(extras, "\n")
}

// Run 执行一轮完整的用户交互（含 ReAct 迭代）：追加用户消息后进入循环。
func (a *Agent) Run(ctx context.Context, sessionID, input string, emit Emitter) error {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return fmt.Errorf("会话不存在: %s", sessionID)
	}

	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, input))
	emit(Event{Type: EventUser, Text: input})
	a.lastUserInput = input // 供 systemPrompt 里技能触发词匹配

	return a.runLoop(ctx, sess, emit)
}

// Regenerate 重新生成最后一轮回复：把会话回退到最近一条用户消息
// （丢弃其后的助手回复与工具结果），随后基于同一条提问重跑循环。
func (a *Agent) Regenerate(ctx context.Context, sessionID string, emit Emitter) error {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return fmt.Errorf("会话不存在: %s", sessionID)
	}

	idx := -1
	for i := len(sess.Messages) - 1; i >= 0; i-- {
		if sess.Messages[i].Role == llm.RoleUser {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("没有可重新生成的用户消息")
	}
	sess.Messages = sess.Messages[:idx+1]

	return a.runLoop(ctx, sess, emit)
}

// runLoop 执行 ReAct 主循环（不追加用户消息，由调用方准备会话上下文）。
func (a *Agent) runLoop(ctx context.Context, sess *Session, emit Emitter) error {
	return a.runLoopWithPersistence(ctx, sess, emit, true)
}

// runLoopEphemeral 执行临时子智能体循环，不把子会话写入用户历史。
func (a *Agent) runLoopEphemeral(ctx context.Context, sess *Session, emit Emitter) error {
	return a.runLoopWithPersistence(ctx, sess, emit, false)
}

func (a *Agent) runLoopWithPersistence(ctx context.Context, sess *Session, emit Emitter, persist bool) error {
	for step := 1; step <= a.cfg.MaxSteps; step++ {
		emit(Event{Type: EventStep, Step: step})
		req := llm.Request{
			System:      a.systemPromptFor(sess),                   // 含会话任务清单（todo 段随进度实时更新）
			Messages:    a.prepareMessages(ctx, sess, emit),        // 超阈值时自动摘要压缩（历史本身不改）
			Tools:       a.registry.DefinitionsFor(a.exposureFn()), // Exposure 层：工具可见面过滤（nil=全量）
			MaxTokens:   a.llmCfg.MaxTokens,                        // 来自模型条目「输出上限」/配置，不再硬编码
			Temperature: a.llmCfg.Temperature,                      // 同上
			Thinking:    ThinkingFromCtx(ctx),
		}

		// 注入重试通知：上游瞬时故障自动重试时，向前端透出「请求失败，正在重试…」。
		// 连「第几次 / 共几次」一起带上 —— 只给一句笼统的「正在重试」，用户无法判断
		// 是偶发抖动还是上游持续故障（实测 429 会连撞满 5 次，界面上必须看得出进度）。
		hookCtx := llm.WithRetryHook(ctx, func(attempt, maxAttempts int, reason string) {
			emit(Event{Type: EventRetry, Error: reason, Attempt: attempt, MaxAttempts: maxAttempts})
		})

		stream, err := a.provider.Stream(hookCtx, req)
		if err != nil {
			emit(Event{Type: EventError, Error: err.Error()})
			a.save(sess, persist)
			return err
		}

		turn, err := a.consumeStream(ctx, sess, stream, emit)
		if err != nil {
			emit(Event{Type: EventError, Error: err.Error()})
			a.save(sess, persist)
			return err
		}

		blocks := make([]llm.ContentBlock, 0, len(turn.ToolCalls)+1)
		if turn.Text != "" {
			blocks = append(blocks, llm.ContentBlock{Type: llm.BlockText, Text: turn.Text})
		}
		for _, tc0 := range turn.ToolCalls {
			// 上游只接受合法函数名（无点号），Definitions 下发的是清洗后的
			// wire 名；这里还原为注册名，后续执行/事件/历史全部用真实名。
			tc := tc0
			tc.Name = a.registry.ResolveWire(tc0.Name)
			blocks = append(blocks, llm.ContentBlock{
				Type:  llm.BlockToolUse,
				ID:    tc.ID,
				Name:  tc.Name,
				Input: tc.Input,
			})
		}
		if len(blocks) > 0 {
			sess.Messages = append(sess.Messages, llm.AssistantBlocksMessage(blocks))
		}

		if len(turn.ToolCalls) == 0 {
			a.save(sess, persist)
			emit(Event{Type: EventDone})
			return nil
		}

		for _, tc0 := range turn.ToolCalls {
			tc := tc0
			tc.Name = a.registry.ResolveWire(tc0.Name)
			decision := a.executor.Evaluate(tc.Name, tc.Input)
			emit(Event{
				Type:       EventToolCall,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolInput:  tc.Input,
				Decision:   string(decision.Decision),
				Reason:     decision.Reason,
				DiffStats:  a.diffStatsFor(tc),
			})

			// 注入会话运行域：后台任务 / 检查点 / 任务清单等按会话归属的行为
			// 依赖 sessionID 与 step（见 pkg/tools/session.go）。
			toolCtx := tools.WithSession(ctx, tools.SessionScope{SessionID: sess.ID, Step: step})
			res, _ := a.executor.Execute(toolCtx, tc.Name, tc.Input)
			if res == nil {
				res = tools.Err("工具无返回")
			}
			emit(Event{Type: EventToolResult, ToolCallID: tc.ID, ToolName: tc.Name, Result: res})

			sess.Messages = append(sess.Messages, llm.ToolResultMessage(tc.ID, renderToolResult(res), !res.Success))
		}
		a.save(sess, persist)
	}

	emit(Event{Type: EventDone})
	a.save(sess, persist)
	return nil
}

// save 持久化会话（persist=false 的临时子智能体会话直接跳过）。
// 失败不再静默吞掉：历史版本用 `_ = a.history.Save(...)`，一旦 SQLite 写入
// 失败（锁冲突、缓存被换出等）就会「对话消失但界面正常」，排查时毫无线索。
func (a *Agent) save(sess *Session, persist bool) {
	if !persist {
		return
	}
	if err := a.history.Save(sess.ID); err != nil {
		log.Printf("[agent] 会话持久化失败（会话=%s）：%v", sess.ID, err)
	}
}

// partialCall 是流式拼装中的工具调用。
type partialCall struct {
	name string
	args strings.Builder
}

// consumeStream 消费流式事件并拼装为一轮助手回复。
// 顺带把 LLM 上报的用量（EventUsage）累计到会话上（内存态，供上下文统计展示）。
func (a *Agent) consumeStream(ctx context.Context, sess *Session, stream <-chan llm.StreamEvent, emit Emitter) (*llm.AssistantTurn, error) {
	turn := &llm.AssistantTurn{}
	parts := map[string]*partialCall{}
	order := make([]string, 0, 4)
	var errMsg string

	for ev := range stream {
		if ctx.Err() != nil {
			break
		}
		switch ev.Type {
		case llm.EventTextDelta:
			turn.Text += ev.Text
			emit(Event{Type: EventText, Text: ev.Text})
		case llm.EventReasoningDelta:
			emit(Event{Type: "reasoning", Text: ev.Text})
		case llm.EventUsage:
			if ev.Usage != nil {
				sess.AddUsage(*ev.Usage)
			}
		case llm.EventToolUseStart:
			if _, ok := parts[ev.ToolUseID]; !ok {
				parts[ev.ToolUseID] = &partialCall{name: ev.ToolName}
				order = append(order, ev.ToolUseID)
			} else if ev.ToolName != "" {
				parts[ev.ToolUseID].name = ev.ToolName
			}
		case llm.EventToolUseDelta:
			if p, ok := parts[ev.ToolUseID]; ok {
				p.args.WriteString(ev.InputDelta)
			}
		case llm.EventError:
			errMsg = ev.Error
			emit(Event{Type: EventError, Error: ev.Error})
		}
	}

	if errMsg != "" && len(order) == 0 && turn.Text == "" {
		return nil, fmt.Errorf("%s", errMsg)
	}

	for _, id := range order {
		p := parts[id]
		args := strings.TrimSpace(p.args.String())
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, llm.ToolCall{
			ID:    id,
			Name:  p.name,
			Input: json.RawMessage(args),
		})
	}
	return turn, nil
}

// renderToolResult 将工具结果序列化为回填给模型的文本。
func renderToolResult(res *tools.ToolResult) string {
	if res == nil {
		return "（无结果）"
	}
	data, err := json.Marshal(res)
	if err != nil {
		return res.Error
	}
	return string(data)
}
