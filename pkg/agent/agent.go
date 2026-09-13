// Package agent 实现 Agent 核心引擎：ReAct 循环、上下文管理与会话历史。
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	EventRetry       = "retry" // 上游瞬时故障自动重试中
	EventDone        = "done"
	EventError       = "error"
)

// Event 是 Agent 推送给前端的统一事件。
type Event struct {
	Type       string            `json:"type"`
	Step       int               `json:"step,omitempty"`
	Text       string            `json:"text,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolName   string            `json:"tool_name,omitempty"`
	ToolInput  json.RawMessage   `json:"tool_input,omitempty"`
	Decision   string            `json:"decision,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Result     *tools.ToolResult `json:"result,omitempty"`
	Error      string            `json:"error,omitempty"`
	DiffStats  *DiffStats        `json:"diff_stats,omitempty"` // 编辑类工具的 +/- 行数（供前端绿增红减展示）
}

// DiffStats 是一次文件编辑的行数统计。
type DiffStats struct {
	Added   int `json:"added"`   // 新增行数（+）
	Removed int `json:"removed"` // 删除行数（-）
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

// WorkDir 返回当前工作目录（会话/记忆的 workspace 隔离键）。
func (a *Agent) WorkDir() string { return a.workDir }

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
			System:      a.systemPrompt(),
			Messages:    Compress(sess.Messages, a.cfg.ContextTokenBudget),
			Tools:       a.registry.Definitions(),
			MaxTokens:   a.llmCfg.MaxTokens,   // 来自模型条目「输出上限」/配置，不再硬编码
			Temperature: a.llmCfg.Temperature, // 同上
			Thinking:    ThinkingFromCtx(ctx),
		}

		// 注入重试通知：上游瞬时故障自动重试时，向前端透出「请求失败，正在重试…」。
		hookCtx := llm.WithRetryHook(ctx, func(attempt int, reason string) {
			emit(Event{Type: EventRetry, Error: reason})
		})

		stream, err := a.provider.Stream(hookCtx, req)
		if err != nil {
			emit(Event{Type: EventError, Error: err.Error()})
			if persist {
				_ = a.history.Save(sess.ID)
			}
			return err
		}

		turn, err := a.consumeStream(ctx, stream, emit)
		if err != nil {
			emit(Event{Type: EventError, Error: err.Error()})
			if persist {
				_ = a.history.Save(sess.ID)
			}
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
			if persist {
				_ = a.history.Save(sess.ID)
			}
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

			res, _ := a.executor.Execute(ctx, tc.Name, tc.Input)
			if res == nil {
				res = tools.Err("工具无返回")
			}
			emit(Event{Type: EventToolResult, ToolCallID: tc.ID, ToolName: tc.Name, Result: res})

			sess.Messages = append(sess.Messages, llm.ToolResultMessage(tc.ID, renderToolResult(res), !res.Success))
		}
		if persist {
			_ = a.history.Save(sess.ID)
		}
	}

	emit(Event{Type: EventDone})
	if persist {
		_ = a.history.Save(sess.ID)
	}
	return nil
}

// partialCall 是流式拼装中的工具调用。
type partialCall struct {
	name string
	args strings.Builder
}

// consumeStream 消费流式事件并拼装为一轮助手回复。
func (a *Agent) consumeStream(ctx context.Context, stream <-chan llm.StreamEvent, emit Emitter) (*llm.AssistantTurn, error) {
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
