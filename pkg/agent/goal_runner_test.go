package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// ---------------------------------------------------------------------------
// 白名单护栏：这三条是本功能唯一的硬防线，必须由测试锁死。
// 靠提示词说「你不要…」是不够的 —— 模型不保证照做。
// ---------------------------------------------------------------------------

func TestGoalVerifierToolSetHasNoWrites(t *testing.T) {
	set := goalVerifierToolSet()
	for _, banned := range []string{
		"write_file", "edit_file", "delete_file", // 只判定，不修复
		"goal_verify",                        // 防递归套娃
		"delegate_subagents", "create_skill", // 它不是主智能体
		"todo_write", "save_memory", // 会污染主会话状态
	} {
		if set[banned] {
			t.Errorf("审查者白名单不得包含 %q", banned)
		}
	}
}

func TestGoalVerifierToolSetHasReadAndRun(t *testing.T) {
	set := goalVerifierToolSet()
	// run_command 必须在：verify 的定义就是「把程序跑起来看」。
	// 少了它就退化成「读代码猜对错」—— 那正是本功能要消灭的假验证。
	for _, need := range []string{"read_file", "list_dir", "search_files", "run_command"} {
		if !set[need] {
			t.Errorf("审查者白名单缺少 %q", need)
		}
	}
}

// 审查者必须继承真审计 —— 它会自主执行命令，不能没有痕迹。
func TestGoalVerifierUsesRealAudit(t *testing.T) {
	reg := tools.NewRegistry()
	auditPath := t.TempDir() + "/audit.jsonl"
	audit, err := security.NewAuditLogger(auditPath)
	if err != nil {
		t.Fatalf("建审计失败: %v", err)
	}
	// ⚠️ 必须关：审计 logger 持着文件句柄，不关的话 t.TempDir() 清理会失败
	// （Windows 上尤其明显：unlinkat 报「另一个进程正在使用此文件」）。
	defer func() { _ = audit.Close() }()

	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, audit, nil, time.Second, 32*1024), NewHistory(st), t.TempDir())
	reg.Register(&countingTool{name: "run_command"})

	child := parent.newGoalVerifier(t.TempDir())
	if child.executor.Audit() == nil {
		t.Fatalf("审查者执行器必须持有真审计 logger（否则它自主执行的命令不留痕）")
	}
	// 真跑一次，确认审计文件里确实落了记录。
	child.executor.Execute(context.Background(), "run_command", json.RawMessage(`{}`))
	data, rerr := readFileString(auditPath)
	if rerr != nil {
		t.Fatalf("读审计文件失败: %v", rerr)
	}
	if !strings.Contains(data, "run_command") {
		t.Errorf("审查者执行的命令应进审计日志，实际内容: %s", data)
	}
}

// countingTool 是一个计数用的空工具。
type countingTool struct {
	name string
	n    int
}

func (c *countingTool) Name() string { return c.name }
func (c *countingTool) Description() string {
	return "测试桩工具"
}
func (c *countingTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (c *countingTool) Execute(context.Context, json.RawMessage) (*tools.ToolResult, error) {
	return tools.Ok("ok"), nil
}

func readFileString(p string) (string, error) {
	data, err := os.ReadFile(p)
	return string(data), err
}

// ---------------------------------------------------------------------------
// 步数预算与超时
// ---------------------------------------------------------------------------

func TestGoalVerifierSteps(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 15},   // 主 loop 异常 → 兜底
		{-1, 15},  // 同上
		{5, 5},    // 小于顶 → 继承
		{30, 30},  // 恰好等于顶
		{500, 30}, // 超顶 → 夹住（失控的代价由用户承担）
	}
	for _, c := range cases {
		if got := goalVerifierSteps(c.in); got != c.want {
			t.Errorf("主 loop %d 步 → 期望审查者 %d 步，实际 %d", c.in, c.want, got)
		}
	}
}

// 审查者步数上限必须小于等于它的工具超时所能支撑的量级 —— 这是粗 sanity：
// 单步都要几十秒时，30 步 × 5 分钟的工具超时没有意义，但反过来步数太小
// 会让「起服务 → 跑命令 → 读日志」这种任务刚起步就停。
func TestGoalVerifierToolTimeoutIsWiderThanDefault(t *testing.T) {
	if goalVerifierToolTimeout <= 120*time.Second {
		t.Errorf("审查者的单工具超时（%v）应宽于默认 120s：起服务/编译都可能超过", goalVerifierToolTimeout)
	}
}

// ---------------------------------------------------------------------------
// 提示词
// ---------------------------------------------------------------------------

// 措辞必须与实授工具集一致，否则模型会反复尝试调用不存在的工具直到步数耗尽。
func TestGoalVerifierPromptMatchesToolset(t *testing.T) {
	sys := goalVerifierSystemPrompt
	if !strings.Contains(sys, "禁止修改、创建、删除任何文件") {
		t.Errorf("系统提示词应明确禁止改文件（与白名单一致）")
	}
	if !strings.Contains(sys, "禁止委派子智能体") {
		t.Errorf("系统提示词应禁止委派（与白名单一致）")
	}
	// 必须要求运行时观察，否则模型会只读代码就下结论
	for _, need := range []string{"实际运行", "证据", "存疑即 FAIL", "VERDICT:"} {
		if !strings.Contains(sys, need) {
			t.Errorf("系统提示词缺少 %q", need)
		}
	}
}

func TestGoalVerifierPromptCarriesPriorFindings(t *testing.T) {
	p := goalVerifierPrompt("登录后不再报错", "上一轮：点了登录仍 500")
	if !strings.Contains(p, "登录后不再报错") {
		t.Errorf("应带上目标原文")
	}
	if !strings.Contains(p, "点了登录仍 500") {
		t.Errorf("应带上上一轮发现（主智能体可能只修了一部分）")
	}
	if !strings.Contains(p, "上一轮你报告的问题") {
		t.Errorf("应标明这段是上一轮的发现")
	}
}

func TestGoalVerifierPromptWithoutPriorFindings(t *testing.T) {
	p := goalVerifierPrompt("登录后不再报错", "")
	if !strings.Contains(p, "登录后不再报错") {
		t.Errorf("应带上目标原文")
	}
	if strings.Contains(p, "上一轮你报告的问题") {
		t.Errorf("首轮不该出现「上一轮」段")
	}
}

// ---------------------------------------------------------------------------
// promptOverride：审查者不该看到主智能体那套插件/技能说明
// ---------------------------------------------------------------------------

func TestPromptOverrideReplacesBase(t *testing.T) {
	a := &Agent{}
	base := a.systemPrompt("普通输入")
	if !strings.Contains(base, "CodeForge") && len(base) == 0 {
		t.Fatalf("默认提示词不应为空")
	}
	a.promptOverride = "你是独立验证审查者。"
	got := a.systemPrompt("普通输入")
	if got != "你是独立验证审查者。" {
		t.Errorf("override 应整体替换基础提示词，实际 %q", got)
	}
	// 技能 / 记忆段也不该再被拼进来
	if strings.Contains(got, "技能") || strings.Contains(got, "记忆") {
		t.Errorf("override 下不应再拼技能/记忆段，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// Verify 的降级路径
// ---------------------------------------------------------------------------

// nil 调度器 / 空父体必须返回 BLOCKED，绝不能返回 PASS。
func TestVerifyDegradesToBlocked(t *testing.T) {
	var v *GoalVerifier
	out := v.Verify(context.Background(), "目标", "")
	if out.Verdict.Passes() {
		t.Errorf("nil 调度器不得判 PASS")
	}
	if out.Verdict != VerdictBlocked {
		t.Errorf("应判 BLOCKED，实际 %q", out.Verdict)
	}
	empty := &GoalVerifier{}
	out2 := empty.Verify(context.Background(), "目标", "")
	if out2.Verdict.Passes() {
		t.Errorf("空父体不得判 PASS")
	}
}

// 审查循环失败（被取消 / 模型不可用）时：不得判 PASS，报告里要带原因。
//
// ⚠️ 刻意用「已取消的 ctx」而不是「provider 传 nil」：主循环拿到 nil provider
// 会在 providerSnapshot().Stream(...) 处**解引用 panic**（agent.go 里没有
// nil 判空），那测的是 panic 不是降级路径。已取消的 ctx 走的是真实分支
// ——用户在审查跑到一半时点了停止。
func TestVerifyReportsRunFailureAsBlocked(t *testing.T) {
	reg := tools.NewRegistry()
	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	// history 不能传 nil：主循环每步都要 systemPromptFor → TodoSection →
	// history.Todos，nil 会解引用炸掉。审查循环同样会走到那里。
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, nil, nil, time.Second, 32*1024), NewHistory(st), t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 审查一开始就已取消

	v := NewGoalVerifier(parent)
	out := v.Verify(ctx, "目标", "")
	if out.Verdict.Passes() {
		t.Errorf("审查过程失败时不得判 PASS")
	}
	if out.Verdict != VerdictBlocked {
		t.Errorf("审查过程失败应判 BLOCKED，实际 %q", out.Verdict)
	}
	if out.Err == nil {
		t.Errorf("应带回错误供诊断")
	}
	if out.Report == "" {
		t.Errorf("应给出可读报告，而不是空的")
	}
}

// 进度事件：审查者本来就是个子智能体，发 SubagentEvent 是如实描述。
// 审查一轮可能要跑几分钟，界面上什么都看不到会让人以为卡死了。
func TestVerifyEmitsSubagentProgress(t *testing.T) {
	reg := tools.NewRegistry()
	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, nil, nil, time.Second, 32*1024), NewHistory(st), t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 审查立即结束，但事件仍应发出来

	var got []tools.SubagentEvent
	ctx = tools.WithSubagentSink(ctx, func(ev tools.SubagentEvent) { got = append(got, ev) })

	NewGoalVerifier(parent).Verify(ctx, "目标", "")

	if len(got) == 0 {
		t.Fatalf("审查者应发出进度事件（否则界面上看不出它在动）")
	}
	first := got[0]
	if first.ID != GoalVerifierID {
		t.Errorf("首个事件应是「开始」，ID=%q 实际 %q", GoalVerifierID, first.ID)
	}
	if first.Mode != GoalVerifierMode {
		t.Errorf("模式应为 %q（前端徽标据此显示「验证」），实际 %q", GoalVerifierMode, first.Mode)
	}
	// 最后一个事件必须是终态，且把判定结论摆出来
	last := got[len(got)-1]
	if last.Status != "failed" && last.Status != "completed" {
		t.Errorf("末个事件应是终态（completed/failed），实际 %q", last.Status)
	}
}

// 没有 sink 时不能 panic（嵌入式/测试场景）。
func TestVerifyWithoutSinkIsSafe(t *testing.T) {
	reg := tools.NewRegistry()
	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, nil, nil, time.Second, 32*1024), NewHistory(st), t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := NewGoalVerifier(parent).Verify(ctx, "目标", "")
	if out.Verdict.Passes() {
		t.Errorf("无 sink 不该影响判定")
	}
}

// modeLabel 必须认识 verify，否则卡片上写着「探索 · goal-verify」比不写更让人困惑。
func TestModeLabelKnowsVerify(t *testing.T) {
	if got := modeLabel(GoalVerifierMode); got != "验证" {
		t.Errorf("verify 模式标签应为「验证」，实际 %q", got)
	}
	if got := modeLabel("implement"); got != "实现" {
		t.Errorf("implement 标签不应被改坏，实际 %q", got)
	}
	if got := modeLabel("explore"); got != "探索" {
		t.Errorf("explore 标签不应被改坏，实际 %q", got)
	}
	if got := modeLabel("别的"); got != "探索" {
		t.Errorf("未知模式应回落「探索」，实际 %q", got)
	}
}

// GoalMaxRounds 直接透传配置的归一化结果。
func TestGoalMaxRounds(t *testing.T) {
	if got := GoalMaxRounds(config.BuiltinPluginsConfig{}); got != config.GoalModeDefaultRounds {
		t.Errorf("未配置应得缺省 %d，实际 %d", config.GoalModeDefaultRounds, got)
	}
	if got := GoalMaxRounds(config.BuiltinPluginsConfig{GoalModeMaxRounds: 999}); got != config.GoalModeMaxRoundsCap {
		t.Errorf("超界应夹到 %d，实际 %d", config.GoalModeMaxRoundsCap, got)
	}
}

// 确认审查者与主智能体共用同一个 llm.Provider（不新建 HTTP 栈、不新建凭据）。
func TestGoalVerifierSharesProvider(t *testing.T) {
	reg := tools.NewRegistry()
	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, nil, nil, time.Second, 32*1024), NewHistory(st), t.TempDir())
	child := parent.newGoalVerifier(t.TempDir())
	// 上下文窗口必须继承：否则小窗口端点会一路堆到 12 万 token 才压缩 → 上游 400
	if child.contextWindow.Load() != parent.contextWindow.Load() {
		t.Errorf("上下文窗口必须继承")
	}
	if child.cfg.MaxSteps != goalVerifierSteps(parent.MaxSteps()) {
		t.Errorf("步数应走审查者预算 %d，实际 %d", goalVerifierSteps(parent.MaxSteps()), child.cfg.MaxSteps)
	}
	if child.promptOverride != goalVerifierSystemPrompt {
		t.Errorf("审查者应换成 judge 提示词")
	}
	var _ llm.Provider = child.providerSnapshot()
}
