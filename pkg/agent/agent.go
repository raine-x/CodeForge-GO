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
	"codeforge/pkg/errs"
	"codeforge/pkg/llm"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// 事件类型（推送给前端）。
const (
	EventUser        = "user"
	EventStep        = "step"
	EventText        = "text"
	EventToolPending = "tool_pending" // 工具调用参数流式生成中（尚未执行）
	EventToolCall    = "tool_call"
	EventToolResult  = "tool_result"
	EventHitlRequest = "hitl_request"
	EventRetry       = "retry"    // 上游瞬时故障自动重试中
	EventCompress    = "compress" // 上下文超阈值，已自动摘要压缩
	EventEdit        = "edit"     // 历史被编辑重发截断（前端据此丢弃下方旧内容）
	EventSteer       = "steer"    // 运行中收到的转向指令已并入上下文（下一个步骤边界生效）
	EventRewind      = "rewind"   // 文件已按检查点回滚
	EventInfo        = "info"     // 一般性提示（纯告知，不改变任何状态）
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
	Diff        string            `json:"diff,omitempty"`         // 编辑类工具的 unified diff（供前端「点击查看改动位置」）
	Compress    *CompressInfo     `json:"compress,omitempty"`     // 上下文压缩明细（仅 EventCompress 携带）
	Rewind      *RewindResult     `json:"rewind,omitempty"`       // 文件回滚结果（仅 EventRewind 携带）
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

// diffPreviewMaxLines 是随 tool_call 事件下发的 diff 行数上限。
//
// 整份文件重写时 diff 可能上千行，不能无上限地塞进 WS 帧；超出部分截断并**明确标注**，
// 而不是静默丢掉（用户点开面板时要看得出「这里还有更多」）。
const diffPreviewMaxLines = 400

// previewFor 对支持 PreviewDiff 的工具调用算出 unified diff 与 +/- 统计。
//
// 一次算两用：统计供卡片的「+N/-M」徽标，diff 文本供「点击查看改动位置」的面板。
// 此前只留统计、把已经算好的 diff 丢掉，于是想看改动就得让前端再猜一次
// （2026-09-22：卡片只显示「运行 xxx」，鼠标放上去是一堆原始参数）。
//
// 返回 (diff, stats)；该工具不支持 diff 或本次无差异时返回 ("", nil)。
func (a *Agent) previewFor(tc llm.ToolCall) (string, *DiffStats) {
	tool, ok := a.registry.Get(tc.Name)
	if !ok {
		return "", nil
	}
	dp, ok := tool.(tools.DiffProvider)
	if !ok {
		return "", nil
	}
	diff, err := dp.PreviewDiff(tc.Input)
	if err != nil || diff == "" {
		return "", nil
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
	return clipDiff(diff, diffPreviewMaxLines), st
}

// clipDiff 按行数截断 diff；被截掉时补一行说明，绝不静默丢内容。
func clipDiff(diff string, maxLines int) string {
	if maxLines <= 0 {
		return diff
	}
	lines := strings.Split(diff, "\n")
	if len(lines) <= maxLines {
		return diff
	}
	kept := make([]string, 0, maxLines+1)
	kept = append(kept, lines[:maxLines]...)
	kept = append(kept, fmt.Sprintf("…（改动过大，仅显示前 %d 行；完整内容请直接打开该文件查看）", maxLines))
	return strings.Join(kept, "\n")
}

// Emitter 是事件发送回调。
type Emitter func(Event)

// Agent 是 ReAct 主循环引擎。
type Agent struct {
	cfg         config.AgentConfig
	maxSteps    atomic.Int64
	provider    llm.Provider
	executor    *tools.Executor
	registry    *tools.Registry
	history     *History
	workDir     string
	llmCfg      config.LLMConfig // 请求参数（MaxTokens/Temperature 等），热切换后更新
	memoryStore *store.Store     // 用户记忆存储（与 History 共用同一 SQLite 库）
	builtinOn   map[string]bool  // 内置插件启用表（key = 插件 ID）

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

	// 中途转向（steering）：按会话暂存「运行中新收到的用户指令」，
	// 在下一个步骤边界并入历史。见 steer.go。
	// 三个字段都由 runMu 保护；map 延迟建表，因为部分调用方直接构造 Agent 字面量。
	runMu   sync.Mutex
	running map[string]bool
	steers  map[string]*steerQueue
}

// New 构造 Agent 引擎。llmCfg 提供请求级参数（MaxTokens/Temperature），
// 与 AgentConfig（循环行为）分开传：前者随「应用/保存」模型热切换更新。
func New(cfg config.AgentConfig, llmCfg config.LLMConfig, provider llm.Provider, executor *tools.Executor, history *History, workDir string) *Agent {
	a := &Agent{
		cfg:      cfg,
		llmCfg:   llmCfg,
		provider: provider,
		executor: executor,
		registry: executor.Registry(),
		history:  history,
		workDir:  workDir,
	}
	a.SetMaxSteps(cfg.MaxSteps)
	return a
}

func (a *Agent) SetMaxSteps(n int) { a.maxSteps.Store(int64(n)) }

func (a *Agent) MaxSteps() int { return int(a.maxSteps.Load()) }

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
// RequestViewTokens 返回会话「实际会送进模型」的估算 tokens（含校准系数）。
//
// 与 ContextStat().Raw 的区别：Raw 是**原始历史**总量，这个是**压缩后**的送模量。
// 服务端提示文案要说「压缩后还占多少」，用这个才不误导。
func (a *Agent) RequestViewTokens(sess *Session) int {
	if sess == nil {
		return 0
	}
	return sess.calibratedEstimate(a.requestView(sess))
}

// SessionOverflowFor 报告会话在**目标窗口** ctxIn 下是否放不下，
// 放不下时返回超出量（tokens），放得下返回 0。
//
// 判定口径是**压缩后的实际送模量**（requestView + 校准系数），不是原始历史。
//
// 为什么不能用原始历史：压缩过的会话原始历史仍然很大，用原始量判会把
// 「已经压好、本来完全跑得动」的会话也判成超窗 —— 服务端据此拦住切换，
// 用户就会遇到「明明刚压缩过，一切换又说超限」（2026-09-21 反馈）。
//
// 供服务端在切换模型前判断要不要先压缩。
func (a *Agent) SessionOverflowFor(sess *Session, ctxIn int) int {
	if sess == nil || ctxIn <= 0 {
		return 0
	}
	used := sess.calibratedEstimate(a.requestView(sess))
	if used > ctxIn {
		return used - ctxIn
	}
	return 0
}

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
	// 可能被任意 goroutine 调用（其它连接的占用刷新、REST）：读快照要持锁。
	// 注意不要在持锁期间再调带锁的方法（如 compressionState），读锁不可重入。
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	st.Raw = EstimateTokens(sess.Messages)
	st.Messages = len(sess.Messages)
	if sess.compressedUpTo > 0 && strings.TrimSpace(sess.summaryText) != "" &&
		sess.compressedUpTo <= len(sess.Messages) {
		st.Compressed, st.Summarized = true, sess.compressedUpTo
	}
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
	return a.prepareMessagesBudget(ctx, sess, emit, a.compressBudget())
}

func (a *Agent) prepareMessagesBudget(ctx context.Context, sess *Session, emit Emitter, budget int) []llm.Message {

	// 会话历史可能被回退（Regenerate 截断）或被整体替换，先自愈压缩状态。
	sess.normalizeCompression()

	view := a.requestView(sess)
	// 用**校准后**的估算判定，而不是裸估算：估算器对代码/JSON 会低估
	//（代码约 3–3.5 字符/token，估算按 4 计），只用裸估算会漏压，
	// 请求带着超窗的体量发出去被上游拒绝。校准系数见 Session.tokenFactor。
	used := sess.calibratedEstimate(view)
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

	sess.mu.Lock()
	sess.summaryText = summary
	sess.compressedUpTo = split
	sess.mu.Unlock()
	a.forgetReads(sess.ID)

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

// forgetReads 在压缩生效后作废该会话的文件阅读登记。
//
// 压缩掉的段落里往往就有 read_file 的原文：此后模型对那份文件的了解只剩摘要，
// 再让它直接覆盖写入，写出来的就是「凭印象重排的一整份文件」。
// 登记一清，模型必须重读那一段才能落笔 —— 多一次定向读取，换回内容不被抹掉。
func (a *Agent) forgetReads(sessionID string) {
	// 只做摘要的轻量 Agent 没有注册表，压缩照常发生，闸门自然无需维护。
	if a.registry == nil {
		return
	}
	for _, tool := range a.registry.List() {
		if g, ok := tool.(tools.ReadGate); ok {
			g.ForgetReads(sessionID)
		}
	}
}

// degradedCompress 是压缩的兜底路径：摘要不可用（调用失败 / 无安全切点 /
// 适配器未就绪）时退回机械压缩，保证请求一定不超窗。
//
// 刻意**不推进 compressedUpTo**：机械压缩是有损的临时手段，不应当被记成
// 「已摘要」。下一轮只要摘要恢复可用，仍会正常走摘要路径。
func (a *Agent) degradedCompress(sess *Session, view []llm.Message, used, budget, split int, emit Emitter) []llm.Message {
	out := Compress(view, budget)
	a.forgetReads(sess.ID)
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
//
// lastInput 是本会话最近一条用户输入（技能触发词匹配用）：按会话传入，
// 不放在 Agent 字段上 —— 并发会话会互相覆盖。
func (a *Agent) systemPrompt(lastInput string) string {
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
	if secs := a.skillSections(lastInput); len(secs) > 0 {
		extras = append(extras, secs...)
	}
	if len(extras) == 0 {
		return base
	}
	return base + "\n\n" + strings.Join(extras, "\n")
}

// Run 执行一轮完整的用户交互（含 ReAct 迭代）：追加用户消息后进入循环。
func (a *Agent) Run(ctx context.Context, sessionID, input string, emit Emitter) error {
	return a.RunWithImages(ctx, sessionID, input, nil, emit)
}

func (a *Agent) RunWithImages(ctx context.Context, sessionID, input string, images []llm.ContentBlock, emit Emitter) error {
	limit := a.MaxSteps()
	// 同会话互斥（跨连接）：WS 的 stop 只能停本连接的旧任务，两个标签页可同时
	// 驱动同一会话。后到者等前者在步骤边界退出，拿不到就报错 —— 否则两个循环
	// 并发改写同一份历史并各自全量覆盖写库（docs/修改.md 记录的 cancel-不-join 隐患）。
	if err := a.beginRunWait(ctx, sessionID); err != nil {
		return err
	}
	defer a.endRun(sessionID)

	sess, ok := a.history.Get(sessionID)
	if !ok {
		return fmt.Errorf("会话不存在: %s", sessionID)
	}

	// 续跑前的历史自愈：上一轮若在「工具执行到一半」被强杀，末尾会挂着一条
	// 没有结果的 tool_use，上游对消息序列有硬约束，带着它请求会被 400 拒绝。
	// 补一条说明性结果（而不是删掉调用记录）——见 Session.repairDanglingToolUse。
	if fixed := sess.repairDanglingToolUse(); len(fixed) > 0 {
		log.Printf("[agent] 会话=%s 补上 %d 条没有结果的工具调用记录（上一轮在工具执行中被中断）",
			sessionID, len(fixed))
		emit(Event{Type: EventInfo,
			Text: fmt.Sprintf("已补上 %d 条被打断的工具调用记录，接着往下跑", len(fixed))})
		a.save(sess, true, emit)
	}

	message := llm.TextMessage(llm.RoleUser, input)
	message.Content = append(message.Content, images...)
	sess.appendMessages(message)
	emit(Event{Type: EventUser, Text: input})
	sess.SetLastUserInput(input) // 供 systemPrompt 里技能触发词匹配

	return a.runLoopWithLimit(ctx, sess, emit, true, limit)
}

// Regenerate 重新生成最后一轮回复：把会话回退到最近一条用户消息
// （丢弃其后的助手回复与工具结果），随后基于同一条提问重跑循环。
func (a *Agent) Regenerate(ctx context.Context, sessionID string, emit Emitter) error {
	limit := a.MaxSteps()
	if err := a.beginRunWait(ctx, sessionID); err != nil {
		return err
	}
	defer a.endRun(sessionID)

	sess, ok := a.history.Get(sessionID)
	if !ok {
		return fmt.Errorf("会话不存在: %s", sessionID)
	}

	sess.mu.Lock()
	// 只认「真正的提问」：steer 插话不是这一轮的提问，按它重跑等于把
	// 用户的「重新生成」落到一句中途指令上。
	idx := lastUserQuestionIndex(sess.Messages)
	if idx < 0 {
		sess.mu.Unlock()
		return fmt.Errorf("没有可重新生成的用户消息")
	}
	sess.Messages = sess.Messages[:idx+1]
	if sess.compressedUpTo > idx {
		sess.compressedUpTo = 0
		sess.summaryText = ""
	}
	var input strings.Builder
	for _, block := range sess.Messages[idx].Content {
		if block.Type == llm.BlockText {
			input.WriteString(block.Text)
		}
	}
	sess.mu.Unlock()
	sess.SetLastUserInput(input.String())

	return a.runLoopWithLimit(ctx, sess, emit, true, limit)
}

// EditAndResend 编辑一条历史用户消息并重跑（newText 不可为空）。
//
// 语义（与「重新生成」同族，但可指定目标消息并改写其内容）：
//
//  1. 在**最近的 msgs 条用户纯文本消息**范围内定位第 back 条（back=0 即最后一条），
//     避免误改很早以前、上下文早已被摘要覆盖的历史；
//  2. 把会话截断到该条用户消息（含），丢弃其后全部助手回复与工具结果；
//  3. 内部回退上下文压缩态 —— compressedUpTo / summaryText 若越过截断点即复位，
//     相当于把「送模视图」也一并退回。⚠️ 刻意**不动** usageIn/usageHit/usageOut：
//     那是上游真实计费口径的累计量（左下角窗口统计的来源），回退历史并不等于
//     这些 token 没花过，抹掉就是伪造账目；
//  4. 用新文本替换该条消息（仅替换文本块，图片等其它块原样保留）；
//  5. 重新跑循环，新回复自然追加在截断点之后 —— 界面上就是「覆盖掉下面的内容」。
//
// 返回被替换消息在历史中的下标与**实际生效的文本**，供 WS 层回报前端。
func (a *Agent) EditAndResend(ctx context.Context, sessionID string, back int, newText string, emit Emitter) (int, error) {
	if strings.TrimSpace(newText) == "" {
		// 「编辑」却没给新内容 = 无效操作（RerunFrom 会把它当成「保留原文」，
		// 那是断点重试的语义，不该被编辑入口误触）。
		return -1, fmt.Errorf("编辑后的内容不能为空")
	}
	idx, _, err := a.RerunFrom(ctx, sessionID, back, newText, emit)
	return idx, err
}

// RerunFrom 是 EditAndResend 的底层实现，也是「断点重试」的服务端入口。
//
// 与 EditAndResend 的唯一差别：newText 为空时**保留原用户消息原文**，只把其后
// 的助手回复与工具结果截掉重跑。被打断 / 报错 / 刷新页面后「没有完整结束」的
// 轮次都走这条路径——用户的提问本身没错，不需要改，也不该被覆盖。
//
// 返回的 text 永远是实际送进历史的那段文本（重试时即原文），WS 层要靠它
// 重建前端视图。
//
// 会在截断之后、跑循环之前 emit 一次 EventEdit（前端据此把视图退到截断点）。
// 顺序很关键：晚于新一轮内容发出的话，前端重建视图会把新回复一起清掉。
func (a *Agent) RerunFrom(ctx context.Context, sessionID string, back int, newText string, emit Emitter) (int, string, error) {
	limit := a.MaxSteps()
	if err := a.beginRunWait(ctx, sessionID); err != nil {
		return -1, "", err
	}
	defer a.endRun(sessionID)

	sess, ok := a.history.Get(sessionID)
	if !ok {
		return -1, "", fmt.Errorf("会话不存在: %s", sessionID)
	}
	keepOriginal := strings.TrimSpace(newText) == ""
	if back < 0 {
		back = 0
	}

	sess.mu.Lock()
	// back 序号与 EditableUserMessages 同源（都只数「真正的提问」），
	// 前端点第 back 条必然落到同一条消息上。
	idx := nthLastUserQuestionIndex(sess.Messages, back)
	if idx < 0 {
		sess.mu.Unlock()
		return -1, "", fmt.Errorf("找不到可编辑的用户消息（仅支持最近 %d 条纯文本提问）", back+1)
	}

	// 截断到该条用户消息：其后的一切（助手回复 / 工具调用与结果）全部丢弃。
	sess.Messages = sess.Messages[:idx+1]
	sess.mu.Unlock()

	// 内部回退压缩态：游标落在截断点之外时整段复位（与 Regenerate 同一处理）。
	// normalizeCompression 自带锁，不能在持锁状态下调用。
	sess.normalizeCompression()
	sess.mu.Lock()
	if sess.compressedUpTo > idx {
		sess.compressedUpTo = 0
		sess.summaryText = ""
	}

	// 取实际生效的文本：重试取原文，编辑取新文本。
	var sb strings.Builder
	for _, block := range sess.Messages[idx].Content {
		if block.Type == llm.BlockText {
			sb.WriteString(block.Text)
		}
	}
	text := sb.String()
	if !keepOriginal {
		text = newText
		// 替换文本块：只改 text，图片等其它内容块保持不动。
		msg := sess.Messages[idx]
		replaced := false
		for i, block := range msg.Content {
			if block.Type != llm.BlockText {
				continue
			}
			msg.Content[i].Text = newText
			replaced = true
			break
		}
		if !replaced {
			msg.Content = append([]llm.ContentBlock{{Type: llm.BlockText, Text: newText}}, msg.Content...)
		}
		sess.Messages[idx] = msg
	}
	sess.mu.Unlock()

	sess.SetLastUserInput(text)

	// ⚠️ 截断的信号必须**在新一轮内容之前**发出。
	//
	// 早先这里不发，由 WS 层在 RerunFrom 返回之后补发 —— 而 RerunFrom 内部已经把
	// 整轮跑完了，新回复早就流到屏幕上。前端收到「历史已截断」再去重建视图，
	// 就把刚流出的回复连同更早的历史一起清掉，屏幕上只剩一条错误信息
	//（2026-09-22 用户反馈：「界面上之前的记录被一条错误覆盖」）。
	emit(Event{Type: EventEdit, Step: idx, Text: text})

	if err := a.runLoopWithLimit(ctx, sess, emit, true, limit); err != nil {
		return idx, text, err
	}
	return idx, text, nil
}

// UnfinishedTurnAnchor 判断「最后一轮是否没有完整结束」，并给出**「继续」**的锚点。
//
// 名字刻意不叫 retry：调用方拿到锚点后做的事是「追加一句『继续』接着跑」，
// 不截断历史、不回退文件（破坏性的重来走「重新生成」/「编辑重发」）。
//
// 判据只看历史形状，不依赖任何内存态（页面刷新、进程重启后都能算）：
//   - 存在最后一条用户纯文本发言，且它之后**没有**任何「纯文本、无工具调用」的
//     助手终稿 → 该轮未完成（打断 / 上游报错 / 崩溃 / 刚发出尚未回复），返回 0；
//   - 已有这样的终稿 → 返回 -1，表示这一轮已经收尾，不需要提示继续。
//
// 返回的是 back（距最后一条用户消息的距离），目前只有 0 / -1 两种取值：
// 只认「最后一轮」，更早的轮次用「编辑」按钮即可。
func (a *Agent) UnfinishedTurnAnchor(sessionID string) int {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return -1
	}
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	idx := lastPlainUserIndex(sess.Messages)
	if idx < 0 {
		return -1
	}
	for _, m := range sess.Messages[idx+1:] {
		if m.Role != llm.RoleAssistant {
			continue
		}
		hasToolUse, hasText := false, false
		for _, b := range m.Content {
			switch b.Type {
			case llm.BlockToolUse:
				hasToolUse = true
			case llm.BlockText:
				if strings.TrimSpace(b.Text) != "" {
					hasText = true
				}
			}
		}
		// 有正文且没有工具调用 = 这一轮已经给出了终稿。
		if hasText && !hasToolUse {
			return -1
		}
	}
	return 0
}

// UserMessageRef 描述一条「可编辑的用户消息」（供前端渲染编辑按钮）。
type UserMessageRef struct {
	Index int    `json:"index"` // 在 sess.Messages 中的下标
	Back  int    `json:"back"`  // 距最后一条用户消息的距离（0 = 最后一条），编辑时回传它
	Text  string `json:"text"`  // 纯文本内容
}

// EditableUserMessages 返回最近的 n 条「用户真正的提问」，按**时间正序**排列
// （最后一条在末尾）。前端只给最近 n 条挂编辑按钮，这里就是那份白名单。
//
// 只认纯文本发言（isPlainUserText）：带 tool_result 的 user 消息是工具回填，
// 不是用户说的话，改它没有意义也会破坏消息序列的合法性。
// 另外排除 steer 插话（isUserQuestion）：插话不该被改写重发，
// 且白名单与 RerunFrom 的 back 序号必须同源，否则点第 back 条会改错消息。
func (a *Agent) EditableUserMessages(sessionID string, n int) []UserMessageRef {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return nil
	}
	if n <= 0 {
		return nil
	}
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	var out []UserMessageRef
	back := 0
	for i := len(sess.Messages) - 1; i >= 0 && len(out) < n; i-- {
		if !isUserQuestion(sess.Messages[i]) {
			continue
		}
		var sb strings.Builder
		for _, b := range sess.Messages[i].Content {
			if b.Type == llm.BlockText {
				sb.WriteString(b.Text)
			}
		}
		out = append(out, UserMessageRef{Index: i, Back: back, Text: sb.String()})
		back++
	}
	// 倒序收集得到的是「由近及远」，翻回时间正序
	for l, r := 0, len(out)-1; l < r; l, r = l+1, r-1 {
		out[l], out[r] = out[r], out[l]
	}
	return out
}

// nthLastPlainUserIndex 返回倒数第 n 条「用户纯文本发言」的下标（n 从 0 起），
// 越界返回 -1。n=0 等价于 lastPlainUserIndex。
func nthLastPlainUserIndex(msgs []llm.Message, n int) int {
	return nthLastUserIndex(msgs, n, isPlainUserText)
}

// nthLastUserQuestionIndex 返回倒数第 n 条「真正的用户提问」的下标（n 从 0 起）。
//
// 「编辑重发」的 back 序号必须与 EditableUserMessages 给出的白名单同源：
// 两处都用这一组，前端点第 back 条就必然落到同一条消息上。
func nthLastUserQuestionIndex(msgs []llm.Message, n int) int {
	return nthLastUserIndex(msgs, n, isUserQuestion)
}

// nthLastUserIndex 是上面两个函数的公共实现（倒序数第 n 条满足 pred 的消息）。
func nthLastUserIndex(msgs []llm.Message, n int, pred func(llm.Message) bool) int {
	if n < 0 {
		return -1
	}
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if !pred(msgs[i]) {
			continue
		}
		if seen == n {
			return i
		}
		seen++
	}
	return -1
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
	return a.runLoopWithLimit(ctx, sess, emit, persist, a.MaxSteps())
}

// injectSteers 把排队中的转向指令作为用户消息并入历史（见 steer.go）。
//
// 注入点是「新步骤开始、上一次工具结果已入账」这个边界：工具结果已经写进历史，
// 模型收到转向指令时看到的是「做到哪一步、拿到了什么」，而不是半截的推理。
// 刻意不掐断正在飞行中的那次请求 —— 那只会留下一条不完整的助手回复。
func (a *Agent) injectSteers(sess *Session, emit Emitter, persist bool) {
	texts := a.drainSteer(sess.ID)
	if len(texts) == 0 {
		return
	}
	a.appendSteers(sess, texts, emit)
	a.save(sess, persist, emit)
}

// appendSteers 把指令作为用户消息并入历史并广播事件（落盘由调用方负责）。
//
// 消息带 OriginSteer 标记：它在历史里必须与「真正的提问」可区分，
// 否则重新生成 / 编辑重发会定位到一句中途插话上，界面回放也分不清两者。
func (a *Agent) appendSteers(sess *Session, texts []string, emit Emitter) {
	for _, t := range texts {
		msg := llm.TextMessage(llm.RoleUser, t)
		msg.Origin = OriginSteer
		sess.appendMessages(msg)
		sess.SetLastUserInput(t) // 技能触发词按最新一条用户输入匹配
	}
	log.Printf("[steer] 会话=%s 已并入 %d 条中途指令", sess.ID, len(texts))
	emit(Event{Type: EventSteer, Text: strings.Join(texts, "\n")})
}

// maxEmptyTurnRetries 是空回合的自动重试次数。
// 设 1 而不是更多：空回合绝大多数是上游瞬时抖动，一次足够；
// 真成性问题（输出上限太小、模型不支持工具调用）重试再多次也只是白等，
// 不如早点把原因摊到用户面前。
const maxEmptyTurnRetries = 1

func (a *Agent) runLoopWithLimit(ctx context.Context, sess *Session, emit Emitter, persist bool, limit int) error {
	// 运行权（同会话互斥）由公开入口在进循环前获取：Run / Regenerate / RerunFrom
	// 调 beginRunWait；子智能体的临时循环（runLoopEphemeral）不注册 —— 它的会话
	// 不进缓存、不接受转向，没有互斥对象。
	//
	// 连续空回合计数（判据与提示见循环内）：任意一轮产出正文或工具调用就归零。
	emptyTurns := 0
	// 步骤号会话级单调：每完成一步恰好追加一条助手消息，因此以「已有助手消息数」
	// 为基准，本轮第 k 步 = stepBase + k。若每轮都从 1 重新计数，后续轮回的同号
	// 检查点会被 (会话,步骤,路径) 主键的 INSERT OR IGNORE 静默丢弃，回滚映射
	// 也随之失真（RewindAfterEdit 靠步骤号对齐消息位置）。
	stepBase := countAssistantMessages(sess.Messages)
	for step := 1; step <= limit; step++ {
		sessStep := stepBase + step
		if err := ctx.Err(); err != nil {
			a.save(sess, persist, emit)
			return err
		}
		a.injectSteers(sess, emit, persist)
		emit(Event{Type: EventStep, Step: sessStep})
		system := a.systemPromptFor(sess)
		definitions := a.registry.DefinitionsFor(a.exposureFn())
		overhead := requestOverhead(system, definitions)

		// 组装并发送本步请求。
		//
		// **上游以「上下文超窗」拒绝时，把压缩线收紧再发一次**，而不是直接返回。
		//
		// 为什么必须这样兜底：压缩判定用的是 EstimateTokens 的**估算**，而估算器
		// 对代码/JSON 会低估（代码约 3–3.5 字符/token，估算按 4 字符/token 计），
		// 压缩线又只留 5% 余量。于是稳定出现「判定没超、真请求超窗」：
		// 输入 134145 + 输出预留 128000 > 窗口 262144，上游回 400
		// upstream_request_rejected，整轮任务白跑（2026-09-21 实测）。
		//
		// 收紧后重发是安全的：压缩是幂等的，且这里不改变用户可见的历史
		//（压缩只影响「送模视图」，sess.Messages 始终完整）。
		var stream <-chan llm.StreamEvent
		shrink := 1.0
		for attempt := 1; ; attempt++ {
			base := a.compressBudget() - overhead
			budget := int(float64(base) * shrink)
			if budget <= 0 {
				a.save(sess, persist, emit)
				return fmt.Errorf("系统提示词和工具定义已占满上下文预算，请减少提示词或工具数量")
			}
			compressedBefore := sess.compressedUpTo
			messages := a.prepareMessagesBudget(ctx, sess, func(ev Event) {
				if ev.Compress != nil {
					ev.Compress.Before += overhead
					ev.Compress.After += overhead
					ev.Compress.Budget += overhead
				}
				emit(ev)
			}, budget)
			// 压缩一旦发生就**立刻落盘**：本轮若中途被打断（进程被杀、用户打断、
			// 上游长时间无响应），压缩成果不该跟着丢 —— 否则下次启动又要重压一遍，
			// 用户看到的就是「每次启动的第一次都炸上下文」（2026-09-21 反馈）。
			if sess.compressedUpTo != compressedBefore {
				a.save(sess, persist, emit)
			}
			if sess.calibratedEstimate(messages) > budget {
				messages = a.degradedCompress(sess, messages, sess.calibratedEstimate(messages), budget, sess.compressedUpTo, emit)
			}
			if sess.calibratedEstimate(messages) > budget {
				a.save(sess, persist, emit)
				return fmt.Errorf("压缩后仍超过上下文预算，请缩短输入或减少工具定义")
			}
			req := llm.Request{
				System:      system,
				Messages:    messages,
				Tools:       definitions,
				MaxTokens:   a.llmCfg.MaxTokens,   // 来自模型条目「输出上限」/配置，不再硬编码
				Temperature: a.llmCfg.Temperature, // 同上
				Thinking:    ThinkingFromCtx(ctx),
			}
			// 记下本次请求的估算总量，供拿到上游真实用量后校准估算器
			//（见 Session.calibrateTokenFactor）。必须**含**系统提示与工具定义，
			// 否则比值口径对不上，校准会偏。
			sess.setReqEstimate(EstimateTokens(messages) + overhead)

			// 注入重试通知：上游瞬时故障自动重试时，向前端透出「请求失败，正在重试…」。
			// 连「第几次 / 共几次」一起带上 —— 只给一句笼统的「正在重试」，用户无法判断
			// 是偶发抖动还是上游持续故障（实测 429 会连撞满 5 次，界面上必须看得出进度）。
			hookCtx := llm.WithRetryHook(ctx, func(attempt, maxAttempts int, reason string) {
				emit(Event{Type: EventRetry, Error: reason, Attempt: attempt, MaxAttempts: maxAttempts})
			})

			s, err := a.provider.Stream(hookCtx, req)
			if err == nil {
				stream = s
				break
			}
			if errs.Classify(err) == errs.KindContextOverflow && attempt <= maxOverflowShrinks {
				// 告知用户「不是你的错，我在自救」——否则界面上只会突然多出一段
				// 摘要，用户不知道发生了什么。
				shrink *= overflowShrinkRatio
				log.Printf("[compress] 会话=%s 上游报上下文超窗，收紧压缩线至 %.0f%% 后重试（第 %d 次）",
					sess.ID, shrink*100, attempt)
				emit(Event{Type: EventCompress, Compress: &CompressInfo{
					Budget:   budget,
					Window:   a.ContextWindow(),
					Degraded: true,
					Reason:   "上游报告上下文超窗，正在压缩历史后重试",
				}})
				continue
			}
			// 不在这里 emit：错误返回给调用方后，WS 层（ws_handler.run）会统一
			// 下发一次 error 事件；这里再 emit 就会显示两遍（如 402 余额不足）。
			a.save(sess, persist, emit)
			return err
		}

		turn, err := a.consumeStream(ctx, sess, stream, emit)
		if err != nil {
			// 半截的一轮也要入账：正文早就通过 EventText 实时推给前端了，
			// 丢掉就会出现「界面上显示着一段历史里不存在的回复」，刷新后凭空消失。
			a.recordPartialTurn(sess, turn, partialTurnReason(ctx, err))
			a.save(sess, persist, emit)
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
				Type:       llm.BlockToolUse,
				ID:         tc.ID,
				Name:       tc.Name,
				Input:      tc.Input,
				ThoughtSig: tc.ThoughtSig,
			})
		}
		if len(blocks) > 0 {
			sess.appendMessages(llm.AssistantBlocksMessage(blocks))
		}

		if turn.Text != "" || len(turn.ToolCalls) > 0 {
			emptyTurns = 0 // 本轮有产出，之前的空回合不再累计
		}

		if len(turn.ToolCalls) == 0 {
			// 收尾前再看一眼队列：模型自认为说完了，但用户可能刚好在这一步按了转向。
			// 这里直接退出等于把用户的话吞掉（endRun 会丢弃未消费的指令），
			// 所以并入之后继续跑下一轮，让指令一定有落点。
			if texts := a.drainSteer(sess.ID); len(texts) > 0 {
				a.appendSteers(sess, texts, emit)
				a.save(sess, persist, emit)
				continue
			}
			// 空回合：既没正文也没工具调用。思考型上游偶尔只回 reasoning_content，
			// 或流被静默截断（实测 glm 连读十余个文件后出现过一次）。
			// 当成「回答完毕」收摊的话，界面上就是任务凭空停了、一句话也没有，
			// 用户只能自己猜是不是额度用完了 —— 先重试一次，仍然空就明确中止。
			if strings.TrimSpace(turn.Text) == "" {
				emptyTurns++
				if emptyTurns <= maxEmptyTurnRetries {
					log.Printf("[agent] 会话=%s 第 %d 步是空回合（思考 %d 字、正文 0 字、工具 0 次），重试",
						sess.ID, step, turn.ReasoningLen)
					emit(Event{Type: EventRetry, Error: "模型本轮没有返回内容，正在重试",
						Attempt: emptyTurns, MaxAttempts: maxEmptyTurnRetries + 1})
					continue
				}
				a.save(sess, persist, emit)
				if turn.ReasoningLen > 0 {
					return fmt.Errorf("模型连续 %d 次只返回思考、没有正文也没有工具调用，本轮中止。"+
						"重发一句「继续」通常就能接上；反复出现请调大设置里的「输出上限」或换个模型", emptyTurns)
				}
				return fmt.Errorf("模型连续 %d 次返回空内容，本轮中止。可重发这句，或换个模型重试", emptyTurns)
			}
			a.save(sess, persist, emit)
			emit(Event{Type: EventDone})
			return nil
		}

		for _, tc0 := range turn.ToolCalls {
			tc := tc0
			tc.Name = a.registry.ResolveWire(tc0.Name)
			decision := a.executor.Evaluate(tc.Name, tc.Input)
			// diff 与统计一次算出：统计给卡片的「+N/-M」徽标，
			// diff 文本给「点击查看改动位置」的面板（见 previewFor）。
			diff, stats := a.previewFor(tc)
			emit(Event{
				Type:       EventToolCall,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolInput:  tc.Input,
				Decision:   string(decision.Decision),
				Reason:     decision.Reason,
				DiffStats:  stats,
				Diff:       diff,
			})

			// 注入会话运行域：后台任务 / 检查点 / 任务清单等按会话归属的行为
			// 依赖 sessionID 与 step（见 pkg/tools/session.go）。
			toolCtx := tools.WithSession(ctx, tools.SessionScope{SessionID: sess.ID, Step: sessStep})
			// 注入检查点槽：写工具在动文件前把旧内容上报，按 (会话,步骤,路径) 落库，
			// 使「回退到某一步之前」成为可能（Plan.md #4）。persist=false 的临时
			// 子智能体循环不记录：它不落历史，回滚点也无从对应。
			if persist {
				toolCtx = tools.WithCheckpointSink(toolCtx, func(ev tools.CheckpointEvent) {
					a.recordCheckpoint(sess.ID, sessStep, ev)
				})
			}
			res, _ := a.executor.Execute(toolCtx, tc.Name, tc.Input)
			if res == nil {
				res = tools.Err("工具无返回")
			}
			emit(Event{Type: EventToolResult, ToolCallID: tc.ID, ToolName: tc.Name, Result: res})

			sess.appendMessages(llm.ToolResultMessage(tc.ID, renderToolResult(res), !res.Success))
			if err := ctx.Err(); err != nil {
				a.save(sess, persist, emit)
				return err
			}
		}
		a.save(sess, persist, emit)
	}

	a.save(sess, persist, emit)
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("已达到工具调用轮数上限（%d 轮），任务尚未完成；可在设置 > 常规中调整 max_steps 后重试", limit)
}

// save 持久化会话（persist=false 的临时子智能体会话直接跳过）。
// 失败不再静默吞掉：历史版本用 `_ = a.history.Save(...)`，一旦 SQLite 写入
// 失败（锁冲突、缓存被换出等）就会「对话消失但界面正常」，排查时毫无线索。
//
// 传入 emit 时，失败会向用户推一条 info 提示（每会话只报第一次，避免 DB
// 故障期间每一步都刷一条）：「界面正常但内容没存上」必须让人看得见。
func (a *Agent) save(sess *Session, persist bool, emit ...Emitter) {
	if !persist {
		return
	}
	if err := a.history.Save(sess.ID); err != nil {
		log.Printf("[agent] 会话持久化失败（会话=%s）：%v", sess.ID, err)
		if len(emit) > 0 && emit[0] != nil && sess.markSaveWarned() {
			emit[0](Event{Type: EventInfo,
				Text: "会话保存失败，本次对话内容可能不会被持久化（详见服务端日志）"})
		}
	}
}

// partialTurnReason 生成「半截回合」里工具结果位上的说明文案。
//
// 这些工具调用从未真正执行过，结果位必须写清原因：一是让模型知道刚才那次
// 调用没成（续跑时不会以为自己已经拿到了结果），二是让用户在历史回放里
// 看得出这一轮是被打断的。
func partialTurnReason(ctx context.Context, err error) string {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "本轮被用户打断，该工具调用未执行"
	}
	return fmt.Sprintf("本轮因上游/传输错误中止，该工具调用未执行：%v", err)
}

// recordPartialTurn 把被打断的一轮（正文 + 已拼装完的工具调用）记入历史。
//
// 两个理由：
//  1. 正文早已通过 EventText 推给前端 —— 不入账就是「界面显示着一段历史里
//     不存在的回复」，刷新即消失，用户会以为丢消息；
//  2. 已拼装完成的 tool_use 若不配 tool_result，历史就是一条违反上游硬约束的
//     序列（tool_use 必须紧跟配对的 tool_result），下一轮请求会被严格上游直接
//     400。它们从未执行，所以一律补「未执行」错误结果。
//
// 没有任何内容（连正文都没有）时不写空消息，保持历史干净。
func (a *Agent) recordPartialTurn(sess *Session, turn *llm.AssistantTurn, reason string) {
	if sess == nil || turn == nil {
		return
	}
	// 上游用思考签名时（Gemini 3），签名是工具调用的一部分：被截断的那次调用
	// 很可能还没等到签名就断了，而缺签名的 tool_use 回送会被上游直接 400
	//（INVALID_ARGUMENT: Function call is missing a thought_signature）。
	// 判据：本轮只要有一条带签名，就说明上游确实在用签名 —— 那么不带签名的那些
	// 宁可丢弃（回到「只留正文」的老行为），也不要写进历史换来一次 400。
	needSig := false
	for _, tc := range turn.ToolCalls {
		if tc.ThoughtSig != "" {
			needSig = true
			break
		}
	}

	blocks := make([]llm.ContentBlock, 0, len(turn.ToolCalls)+1)
	if strings.TrimSpace(turn.Text) != "" {
		blocks = append(blocks, llm.ContentBlock{Type: llm.BlockText, Text: turn.Text})
	}
	recorded := make([]llm.ToolCall, 0, len(turn.ToolCalls))
	for _, tc0 := range turn.ToolCalls {
		if needSig && tc0.ThoughtSig == "" {
			continue
		}
		// 与正常路径一致：上游看到的是清洗后的 wire 名，历史里存注册名。
		tc := tc0
		if a.registry != nil {
			tc.Name = a.registry.ResolveWire(tc0.Name)
		}
		blocks = append(blocks, llm.ContentBlock{
			Type:       llm.BlockToolUse,
			ID:         tc.ID,
			Name:       tc.Name,
			Input:      tc.Input,
			ThoughtSig: tc.ThoughtSig,
		})
		recorded = append(recorded, tc)
	}
	if len(blocks) == 0 {
		return
	}
	sess.appendMessages(llm.AssistantBlocksMessage(blocks))
	for _, tc := range recorded {
		sess.appendMessages(llm.ToolResultMessage(tc.ID, reason, true))
	}
}

// partialCall 是流式拼装中的工具调用。
type partialCall struct {
	name string
	args strings.Builder
	// sig 累加上游随该工具调用下发的「思考签名」。它必须跟着这条调用一起
	// 存进历史并原样回送，否则 Gemini 一类上游会在下一轮直接 400。
	sig strings.Builder
}

// consumeStream 消费流式事件并拼装为一轮助手回复。
// 顺带把 LLM 上报的用量（EventUsage）累计到会话上（内存态，供上下文统计展示）。
func (a *Agent) consumeStream(ctx context.Context, sess *Session, stream <-chan llm.StreamEvent, emit Emitter) (*llm.AssistantTurn, error) {
	turn := &llm.AssistantTurn{}
	parts := map[string]*partialCall{}
	order := make([]string, 0, 4)
	var errMsg string

streamLoop:
	for {
		var ev llm.StreamEvent
		select {
		case <-ctx.Done():
			return turn, ctx.Err()
		case next, ok := <-stream:
			if !ok {
				break streamLoop
			}
			ev = next
		}
		if err := ctx.Err(); err != nil {
			return turn, err
		}
		switch ev.Type {
		case llm.EventTextDelta:
			turn.Text += ev.Text
			emit(Event{Type: EventText, Text: ev.Text})
		case llm.EventReasoningDelta:
			turn.ReasoningLen += len([]rune(ev.Text))
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
			// 工具调用参数开始生成：立刻通知前端（大参数如整页写入要生成
			// 数十 KB，期间若只靠 tool_call 事件，界面会几分钟毫无反馈）。
			// 只在首个工具开始时发一次，避免多工具并行时重复打扰。
			if len(order) == 1 {
				emit(Event{Type: EventToolPending, ToolName: ev.ToolName})
			}
		case llm.EventToolUseDelta:
			if p, ok := parts[ev.ToolUseID]; ok {
				if ev.ThoughtSig != "" {
					p.sig.WriteString(ev.ThoughtSig)
				} else {
					p.args.WriteString(ev.InputDelta)
				}
			}
		case llm.EventError:
			errMsg = ev.Error
			// 不在此 emit：致命错误返回给调用方（WS 层）会统一发一次，
			// 在这发就会显示两遍。部分内容被截断的情况见函数末尾补一条。
		}
	}

	// 先把已拼装的工具调用装好，再判错。
	//
	// 顺序很关键：上游中途报错 / 用户打断时，这些调用已经生成完毕（参数可能被
	// 截断，用 "{}" 兜底），调用方要拿它们入账（见 recordPartialTurn）——
	// 提前返回会让「界面上正在生成的工具卡片」在历史里凭空消失，
	// 续跑时模型也不知道自己刚才想干什么。
	for _, id := range order {
		p := parts[id]
		args := strings.TrimSpace(p.args.String())
		if args == "" || !json.Valid([]byte(args)) {
			args = "{}"
		}
		turn.ToolCalls = append(turn.ToolCalls, llm.ToolCall{
			ID:         id,
			Name:       p.name,
			Input:      json.RawMessage(args),
			ThoughtSig: strings.TrimSpace(p.sig.String()),
		})
	}

	if err := ctx.Err(); err != nil {
		return turn, err
	}
	if errMsg != "" {
		return turn, fmt.Errorf("%s", errMsg)
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
