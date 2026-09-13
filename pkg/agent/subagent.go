package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
)

// MaxSubagents 是一次委派允许启动的最大子智能体数。
const MaxSubagents = 5

// SubagentRunner 调度互不重叠的临时子智能体。
type SubagentRunner struct {
	parent *Agent
}

// NewSubagentRunner 构造绑定到主 Agent 的子智能体调度器。
func NewSubagentRunner(parent *Agent) *SubagentRunner {
	return &SubagentRunner{parent: parent}
}

// RunSubagents 并发执行子任务；探索任务只获得只读工具，临时会话不写入用户历史。
func (r *SubagentRunner) RunSubagents(ctx context.Context, tasks []tools.SubagentTask) ([]tools.SubagentResult, error) {
	if r == nil || r.parent == nil {
		return nil, fmt.Errorf("子智能体调度器未初始化")
	}
	if err := validateSubagentTasks(tasks); err != nil {
		return nil, err
	}

	results := make([]tools.SubagentResult, len(tasks))
	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task tools.SubagentTask) {
			defer wg.Done()
			results[i] = r.runOne(ctx, task)
		}(i, task)
	}
	wg.Wait()
	return results, nil
}

func (r *SubagentRunner) runOne(ctx context.Context, task tools.SubagentTask) tools.SubagentResult {
	result := tools.SubagentResult{ID: task.ID, Mode: task.Mode, Status: "completed"}
	child := r.parent.newSubagent(task.Mode)
	prompt := subagentPrompt(task)
	sess := &Session{ID: "subagent-" + task.ID, Workspace: r.parent.workDir, Messages: nil}
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, prompt))
	child.lastUserInput = prompt

	// 注入子智能体路径围栏：工具层强制只允许访问 task.Paths 范围内的路径。
	ctx = tools.WithSubagentScope(ctx, tools.SubagentScope{Allowed: task.Paths, Mode: task.Mode})

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
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "failed",
			Detail: "子任务失败", Summary: truncateTail(err.Error(), 300)})
	} else {
		emitEvent(tools.SubagentEvent{ID: task.ID, Mode: task.Mode, Status: "completed",
			Detail: "子任务完成", Summary: truncateTail(result.Summary, 300)})
	}
	result.Summary = strings.TrimSpace(summary.String())
	if result.Summary == "" && result.Error == "" {
		result.Summary = "子智能体完成，但没有返回文本摘要"
	}
	return result
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
	registry := tools.NewRegistry()
	allowed := subagentToolSet(mode)
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
	if cfg.MaxSteps > 8 || cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 8
	}
	child := New(cfg, a.llmCfg, a.provider, executor, a.history, a.workDir)
	child.memoryStore = a.memoryStore
	child.builtinOn = map[string]bool{}
	for k, v := range a.builtinOn {
		child.builtinOn[k] = v
	}
	return child
}

func subagentPrompt(task tools.SubagentTask) string {
	paths := append([]string(nil), task.Paths...)
	sort.Strings(paths)
	scope := "未声明具体路径；只做与其它子任务不重叠的工作"
	if len(paths) > 0 {
		scope = strings.Join(paths, ", ")
	}
	if task.Mode == "explore" {
		return fmt.Sprintf("你是 CodeForge 的只读代码探索子智能体。\n任务：%s\n关注范围：%s\n严格约束：只能调用只读工具（读取文件、列目录、搜索）；不要写文件、编辑文件、删除文件、运行命令、调用其它子智能体。输出简洁的证据、涉及文件/函数和结论，供主智能体汇总。", task.Prompt, scope)
	}
	return fmt.Sprintf("你是 CodeForge 的实现子智能体。\n任务：%s\n允许关注/修改的路径范围：%s\n严格约束：只处理声明范围，不修改其它子任务范围；不要调用其它子智能体。完成后说明改了哪些文件、验证了什么、遗留什么问题。", task.Prompt, scope)
}

func validateSubagentTasks(tasks []tools.SubagentTask) error {
	if len(tasks) == 0 {
		return fmt.Errorf("至少需要一个子任务")
	}
	if len(tasks) > MaxSubagents {
		return fmt.Errorf("一次最多只能委派 %d 个子智能体", MaxSubagents)
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

// subagentToolSet 返回子智能体允许使用的工具白名单（按模式）。
// 原则：只暴露代码工作所必需的文件工具，绝不暴露 shell 运行、插件/外部工具、
// 技能创建或其它子智能体委派工具，从源头消除越权与递归委派风险。
func subagentToolSet(mode string) map[string]bool {
	set := map[string]bool{
		"read_file":    true,
		"list_dir":     true,
		"search_files": true,
		"save_memory":  true,
	}
	if mode == "implement" {
		set["write_file"] = true
		set["edit_file"] = true
		set["delete_file"] = true
	}
	return set
}
