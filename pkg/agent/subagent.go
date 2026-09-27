package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
)

// MaxSubagents 是一次委派允许启动的最大子智能体数（硬上限）。
// 与 config.SubagentConcurrencyCap 同源：配置只能把并发调小，不能突破它。
const MaxSubagents = config.SubagentConcurrencyCap

// SubagentPolicy 是子智能体的运行策略（由 config.subagents 派生，设置页可热更新）。
//
// 设计取向是「只收不放」：所有开关都只能缩小能力，不能扩大 —— 子智能体的工具
// 白名单本身就不含 shell、插件、技能创建与递归委派，策略再叠加一层收窄。
type SubagentPolicy struct {
	MaxConcurrent int  // 一次委派最多并行几个子智能体（1..MaxSubagents）
	MaxSteps      int  // 单个子智能体的 ReAct 步数上限（<=0 = 继承主 loop）
	AllowWrite    bool // implement 子智能体是否可写入/编辑文件
	AllowDelete   bool // 是否可删除文件（AllowWrite=false 时无意义）
	AllowMemory   bool // 是否可写入用户记忆（save_memory）
}

// DefaultSubagentPolicy 返回与历史行为完全一致的默认策略。
func DefaultSubagentPolicy() SubagentPolicy {
	return SubagentPolicy{
		MaxConcurrent: MaxSubagents,
		MaxSteps:      0, // 0 = 继承主 loop 的 max_steps
		AllowWrite:    true,
		AllowDelete:   true,
		AllowMemory:   true,
	}
}

// Normalize 收敛越界值，保证策略在任何情况下都可用。
func (p SubagentPolicy) Normalize() SubagentPolicy {
	if p.MaxConcurrent <= 0 || p.MaxConcurrent > MaxSubagents {
		p.MaxConcurrent = MaxSubagents
	}
	// MaxSteps<=0 是「继承主 loop」的合法取值，原样保留；
	// 越界只可能来自手改 yaml，按硬上限收敛。
	if p.MaxSteps > config.SubagentStepCap {
		p.MaxSteps = config.SubagentStepCap
	}
	return p
}

// NewSubagentPolicy 从全局配置派生运行策略。
func NewSubagentPolicy(cfg config.Config) SubagentPolicy {
	return SubagentPolicy{
		MaxConcurrent: cfg.SubagentMaxConcurrent(),
		MaxSteps:      cfg.SubagentMaxSteps(cfg.Agent.MaxSteps),
		AllowWrite:    cfg.SubagentAllowWrite(),
		AllowDelete:   cfg.SubagentAllowDelete(),
		AllowMemory:   cfg.SubagentAllowMemory(),
	}.Normalize()
}

// SubagentRunner 调度互不重叠的临时子智能体。
type SubagentRunner struct {
	parent *Agent
}

// NewSubagentRunner 构造绑定到主 Agent 的子智能体调度器。
//
// 调度器每次运行都从 parent 现读策略（而非缓存），因此设置页改动对
// 已经构造好的工具实例立即生效，无需重建 runner。
func NewSubagentRunner(parent *Agent) *SubagentRunner {
	return &SubagentRunner{parent: parent}
}

// RunSubagents 并发执行子任务；探索任务只获得只读工具，临时会话不写入用户历史。
func (r *SubagentRunner) RunSubagents(ctx context.Context, tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
	if r == nil || r.parent == nil {
		return nil, fmt.Errorf("子智能体调度器未初始化")
	}
	policy := r.parent.SubagentPolicy()
	if err := validateSubagentTasksMax(tasks, policy.MaxConcurrent); err != nil {
		return nil, err
	}

	results := make([]tools.SubagentResult, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task tools.SubagentTask) {
			defer wg.Done()
			results[i] = r.runOne(ctx, task, policy)
		}(i, task)
	}
	wg.Wait()
	return results, nil
}

func (r *SubagentRunner) runOne(ctx context.Context, task tools.SubagentTask, policy SubagentPolicy) tools.SubagentResult {
	result := tools.SubagentResult{ID: task.ID, Mode: task.Mode, Status: "completed"}
	// 工作目录取一次快照，子智能体与子会话共用同一个版本（见 newSubagentIn）：
	// 本函数跑在 per-task goroutine 上，直接读 r.parent.workDir 会与设置页的
	// 热切换并发（string 是两字头，撕裂读会拿到野指针）。
	workDir := r.parent.WorkDir()
	child := r.parent.newSubagentIn(task.Mode, workDir)
	prompt := subagentPrompt(task, policy)
	sess := &Session{ID: "subagent-" + task.ID, Workspace: workDir, Messages: nil}
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, prompt))
	sess.SetLastUserInput(prompt)

	// 注入子智能体路径围栏：工具层强制只允许访问 task.Paths 范围内的路径。
	// TaskID 一并带上：并行委派时界面上会有多张一模一样的审批卡，
	// 用户需要能看出「这张是哪个子任务要的」（见 tools.SubagentScope.TaskID）。
	ctx = tools.WithSubagentScope(ctx, tools.SubagentScope{
		Allowed: task.Paths,
		Mode:    task.Mode,
		TaskID:  task.ID,
	})

	// 事件流：把子智能体的实时进度转发给前端（无 sink 时静默丢弃）。
	sink, hasSink := tools.SubagentSinkFrom(ctx)
	emitEvent := func(ev tools.SubagentEvent) {
		if hasSink && sink != nil {
			sink(ev)
		}
	}
	emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "started",
		Detail: fmt.Sprintf("子任务 %s（%s）已启动", task.ID, modeLabel(task.Mode))})

	var summary strings.Builder
	err := child.runLoopEphemeral(ctx, sess, func(ev Event) {
		switch ev.Type {
		case EventText, "reasoning":
			summary.WriteString(ev.Text)
		case EventToolCall:
			emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "running",
				Detail: fmt.Sprintf("正在调用 %s", ev.ToolName)})
		}
	})
	// 用量先并回父会话再判成败：子会话 persist=false，不落历史，
	// 用量不并回来就随会话一起蒸发，左下角统计会系统性低估委派类任务的花费。
	// 失败的子任务同样花了 token，一并计入。
	r.mergeUsage(ctx, sess)

	// 先算出 summary 再发终态事件：completed 事件要携带摘要给前端展示。
	result.Summary = strings.TrimSpace(summary.String())
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "failed",
			Detail: "子任务失败", Summary: truncateTail(err.Error(), 300)})
		return result
	}
	if result.Summary == "" {
		result.Summary = "子智能体完成，但没有返回文本摘要"
	}
	emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "completed",
		Detail: "子任务完成", Summary: truncateTail(result.Summary, 300)})
	return result
}

// mergeUsage 把子会话的用量并回发起委派的父会话。
//
// 父会话 ID 从 ctx 的会话运行域取（主循环在工具调用前注入，见
// tools.WithSession）；取不到时（脱离会话的调用）静默跳过 —— 用量只是统计口径，
// 不该因为它让子任务失败。
func (r *SubagentRunner) mergeUsage(ctx context.Context, child *Session) {
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return
	}
	parent, ok := r.parent.history.Get(sc.SessionID)
	if !ok {
		return
	}
	in, hit, out := child.UsageSnapshot()
	parent.mergeUsage(in, hit, out)
}

// modeLabel 返回子任务模式的中文标签。
func modeLabel(mode string) string {
	if mode == "implement" {
		return "实现"
	}
	return "探索"
}

// truncateTail 截断过长文本，保留结尾（错误信息的关键部分通常在末尾）。
func truncateTail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (a *Agent) newSubagent(mode string) *Agent {
	return a.newSubagentIn(mode, a.WorkDir())
}

// newSubagentIn 是 newSubagent 的显式工作目录版本。
//
// 调用方需要「子智能体的配置」与「子会话的 Workspace」来自同一个版本时走这个：
// 两次分别取 WorkDir() 会读到两次热更新的中间态，于是子智能体的落盘根目录
// 与它自己的会话 Workspace 分属两个工作区。
func (a *Agent) newSubagentIn(mode string, workDir string) *Agent {
	registry := tools.NewRegistry()
	// 策略只取一次：白名单与步数上限必须来自同一个版本，否则设置页刚好在
	// 这一瞬间改并发/步数，子智能体会拿到「按旧策略挑工具、按新步数跑」的组合。
	sp := a.SubagentPolicy()
	allowed := subagentToolSetPolicy(mode, sp)
	for _, tool := range a.registry.List() {
		// 子智能体只允许使用受控白名单工具集：绝不暴露 delegate_subagents（防递归委派）
		// 与插件/外部工具（越权风险）；implement 限文件工具集，explore 仅只读子集。
		if allowed[tool.Name()] {
			registry.Register(tool)
		}
	}
	policy := a.executor.Policy()
	executor := tools.NewExecutor(registry, policy, nil, nil, 120*time.Second, 32*1024)
	cfg := a.cfg
	// 步数上限：策略里显式配了就用它，没配（MaxSteps<=0）就继承主 loop 的值。
	//
	// 早先这里写死 8，与主 loop 的 max_steps 毫无关系：探索类任务（读几个文件、
	// 搜几处、交叉验证）经常要十几步，8 步会在半途硬停，模型被迫交一份
	// 「还没看完」的结论，主智能体再接着做等于把活儿又干一遍。
	// 现在口径与主智能体一致，成本需要压时在设置页单独调 subagents.max_steps。
	cfg.MaxSteps = resolveSubagentSteps(sp, a.MaxSteps())
	// 四个热更新字段各取一次快照：子智能体在别的 goroutine 上跑，
	// 而设置页随时可能热替换它们（见 Agent.rtMu）。child 此时尚未共享出去，
	// 写它自己的字段不需要加锁。
	child := New(cfg, a.llmCfgSnapshot(), a.providerSnapshot(), executor, a.history, workDir)
	child.memoryStore = a.memoryStore
	// ⚠️ 内置插件开关**不继承**：子智能体的工具白名单里没有 create_skill /
	// delegate_subagents，继承了就会让 System Prompt 注入「你可以创建技能 /
	// 可以委派子智能体」这类说明，而模型手里根本没有对应工具 —— 措辞与实授
	// 工具集不一致，模型会反复尝试调用不存在的工具直到步数耗尽。
	child.builtinOn = map[string]bool{}
	// ⚠️ 上下文窗口**必须继承**：contextWindow 是 Agent 上的 atomic，
	// New() 不会填，子智能体拿到的就是 0 → compressBudget() 回退到写死的
	// context_token_budget（120000）。模型真实窗口若小于该值（32k / 64k 端点），
	// 子智能体会一路堆到 12 万 token 才压缩 → 上游 400 context overflow →
	// 收缩两次仍失败 → 子任务直接挂掉，主智能体只拿到一句「子任务失败」。
	child.contextWindow.Store(int64(a.ContextWindow()))
	// 工具可见面同样继承：用户隐藏过的工具（config.agent.hidden_tools）
	// 不该又出现在子智能体的工具定义里。
	child.SetExposure(a.exposureFn())
	return child
}

// resolveSubagentSteps 解析子智能体的步数上限：策略显式配置优先，否则继承主 loop。
func resolveSubagentSteps(sp SubagentPolicy, parent int) int {
	if sp.MaxSteps > 0 {
		if sp.MaxSteps > config.SubagentStepCap {
			return config.SubagentStepCap
		}
		return sp.MaxSteps
	}
	if parent <= 0 {
		return 8 // 主 loop 也没配（配置异常）：给个能跑的兜底值
	}
	return parent
}

// subagentPrompt 生成子智能体的任务提示词。
// 措辞必须与「实际授予的工具集」一致：被策略禁写时不能还说「你是实现子智能体」，
// 否则模型会反复尝试调用不存在的写工具而空转。
func subagentPrompt(task tools.SubagentTask, policy SubagentPolicy) string {
	paths := append([]string(nil), task.Paths...)
	sort.Strings(paths)
	scope := "未声明具体路径；只做与其它子任务不重叠的工作"
	if len(paths) > 0 {
		scope = strings.Join(paths, ", ")
	}
	const readTips = "探索方式：先广后窄，多换关键词与命名习惯；已知路径直接读，控制读取量，不要把大段原文带回来。"
	const readonlyRule = "严格约束：只能调用只读工具（读取文件、列目录、搜索）；不要写文件、编辑文件、删除文件、运行命令、调用其它子智能体。"
	if task.Mode == "explore" {
		return fmt.Sprintf("你是 CodeForge 的只读代码探索子智能体。\n任务：%s\n关注范围：%s\n%s\n%s\n"+
			"产出要求：结论 + 依据（文件:行号）+ 涉及的关键函数/结构，简洁即可，供主智能体汇总。",
			task.Prompt, scope, readTips, readonlyRule)
	}
	if !policy.AllowWrite {
		return fmt.Sprintf("你是 CodeForge 的子智能体。当前被限制为只读，不能修改任何文件。\n任务：%s\n关注范围：%s\n%s\n%s\n"+
			"产出要求：给出需要主智能体落地的改动清单（文件:行号 + 具体修改点 + 验证方式），由主智能体执行。",
			task.Prompt, scope, readTips, readonlyRule)
	}
	ban := "不要调用其它子智能体。"
	if !policy.AllowDelete {
		ban = "不要删除文件（需要删除时请在结论中说明，由主智能体执行）；不要调用其它子智能体。"
	}
	return fmt.Sprintf("你是 CodeForge 的实现子智能体。\n任务：%s\n允许关注/修改的路径范围：%s\n%s\n"+
		"改动要求：先读后改，遵循项目已有风格；保持最小改动，不顺手重构、不越界。\n"+
		"严格约束：只处理声明范围，不修改其它子任务范围；%s完成后说明改了哪些文件（文件:行号）、验证了什么、遗留什么问题。",
		task.Prompt, scope, readTips, ban)
}

// validateSubagentTasks 以硬上限校验任务集合（保持既有签名，供测试与历史调用方使用）。
func validateSubagentTasks(tasks []tools.SubagentTask) error {
	return validateSubagentTasksMax(tasks, MaxSubagents)
}

// validateSubagentTasksMax 以 max 为并发上限校验任务集合。
//
// 三道校验都在调度之前完成，不依赖模型自觉：
// 数量上限 → id 唯一且必填 → 实现任务必须声明 paths 且彼此不重叠。
func validateSubagentTasksMax(tasks []tools.SubagentTask, max int) error {
	if max <= 0 || max > MaxSubagents {
		max = MaxSubagents
	}
	if len(tasks) == 0 {
		return fmt.Errorf("至少需要一个子任务")
	}
	if len(tasks) > max {
		return fmt.Errorf("一次最多只能委派 %d 个子智能体", max)
	}
	seen := map[string]bool{}
	for _, task := range tasks {
		if task.ID == "" || seen[task.ID] {
			return fmt.Errorf("子任务 id 不能为空且必须唯一: %q", task.ID)
		}
		seen[task.ID] = true
		if strings.TrimSpace(task.Prompt) == "" {
			return fmt.Errorf("子任务 %q 缺少 prompt", task.ID)
		}
		if task.Mode != "explore" && task.Mode != "implement" {
			return fmt.Errorf("子任务 %q 的 mode 必须是 explore 或 implement", task.ID)
		}
		if task.Mode == "implement" && len(task.Paths) == 0 {
			return fmt.Errorf("实现子任务 %q 必须声明 paths，防止与其它子任务操作同一代码", task.ID)
		}
	}
	for i := 0; i < len(tasks); i++ {
		for j := i + 1; j < len(tasks); j++ {
			if scopesOverlap(tasks[i].Paths, tasks[j].Paths) {
				return fmt.Errorf("子任务 %q 与 %q 的代码范围重叠，拒绝并行", tasks[i].ID, tasks[j].ID)
			}
		}
	}
	return nil
}

func scopesOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			x = cleanScope(x)
			y = cleanScope(y)
			if x == y || strings.HasPrefix(x, y+"/") || strings.HasPrefix(y, x+"/") {
				return true
			}
		}
	}
	return false
}

func cleanScope(p string) string {
	p = filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
	p = strings.TrimPrefix(p, "./")
	return strings.ToLower(p)
}

// subagentToolSet 返回子智能体允许使用的工具白名单（按模式，默认策略）。
// 保留此签名供既有调用方与测试使用；带策略的版本是 subagentToolSetPolicy。
func subagentToolSet(mode string) map[string]bool {
	return subagentToolSetPolicy(mode, DefaultSubagentPolicy())
}

// subagentToolSetPolicy 返回子智能体允许使用的工具白名单（按模式 + 能力策略）。
//
// 原则：只暴露代码工作所必需的文件工具，绝不暴露 shell 运行、插件/外部工具、
// 技能创建或其它子智能体委派工具，从源头消除越权与递归委派风险；
// 策略只在此基础上做减法（禁写 / 禁删 / 禁写记忆）。
func subagentToolSetPolicy(mode string, policy SubagentPolicy) map[string]bool {
	set := map[string]bool{
		"read_file":    true,
		"list_dir":     true,
		"search_files": true,
	}
	if policy.AllowMemory {
		set["save_memory"] = true
	}
	if mode == "implement" && policy.AllowWrite {
		set["write_file"] = true
		set["edit_file"] = true
		if policy.AllowDelete {
			set["delete_file"] = true
		}
	}
	return set
}
