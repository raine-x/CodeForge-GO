package agent

import (
	"context"
	"strings"
	"testing"

	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
)

// usageProvider 每轮都上报一次用量，用于验证「子智能体的花费有没有进统计」。
type usageProvider struct{ usage llm.Usage }

func (p *usageProvider) Name() string { return "usage-stub" }

func (p *usageProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 3)
	ch <- llm.StreamEvent{Type: llm.EventUsage, Usage: &p.usage}
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "结论"}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
}

// TestSubagentUsageMergesIntoParent 委派子任务的上游用量必须并回父会话。
//
// 子会话 persist=false、不落历史，用量若不并回来就随会话一起蒸发 ——
// 左下角统计会系统性低估委派类任务的花费（注释里写明 usage 是「上游真实计费口径」）。
func TestSubagentUsageMergesIntoParent(t *testing.T) {
	a := newEmitTestAgent(t, &usageProvider{usage: llm.Usage{
		InputTokens: 300, CachedTokens: 100, OutputTokens: 20}})
	parent, err := a.History().Create("", "父会话")
	if err != nil {
		t.Fatal(err)
	}

	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: parent.ID, Step: 1})
	if _, err := NewSubagentRunner(a).RunSubagents(ctx, []tools.SubagentTask{
		{ID: "t1", Prompt: "探索 pkg/agent", Mode: "explore", Paths: []string{"pkg/agent"}},
	}); err != nil {
		t.Fatalf("委派失败: %v", err)
	}

	in, hit, out := parent.UsageSnapshot()
	if in != 300 || hit != 100 || out != 20 {
		t.Fatalf("子智能体用量应并回父会话，实际 in=%d hit=%d out=%d", in, hit, out)
	}
}

// TestMergeUsageDoesNotCalibrate 并用量不得顺手校准父会话的估算器：
// tokenFactor 是「本会话自己的估算 vs 上游真实」配出来的比值，
// 拿子会话的真实值去除父会话的 reqEstimate 是张冠李戴。
func TestMergeUsageDoesNotCalibrate(t *testing.T) {
	a, h, _ := newCheckpointAgent(t)
	parent, err := h.Create("", "父会话")
	if err != nil {
		t.Fatal(err)
	}
	// 父会话刚发过一次「估算 10 token」的请求：若走 AddUsage，1000 的输入
	// 会被当成 100 倍偏差写进 tokenFactor（并被上限截断成 maxTokenFactor）。
	parent.setReqEstimate(10)

	child := &Session{ID: "subagent-t1"}
	child.AddUsage(llm.Usage{InputTokens: 1000, CachedTokens: 400, OutputTokens: 50})

	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: parent.ID, Step: 1})
	NewSubagentRunner(a).mergeUsage(ctx, child)

	in, hit, out := parent.UsageSnapshot()
	if in != 1000 || hit != 400 || out != 50 {
		t.Fatalf("用量应累加，实际 in=%d hit=%d out=%d", in, hit, out)
	}
	parent.mu.RLock()
	factor := parent.tokenFactor
	parent.mu.RUnlock()
	if factor != 0 {
		t.Fatalf("并用量不得校准父会话的估算系数，实际 %v", factor)
	}
}

// TestMergeUsageWithoutSessionScope 没有会话运行域时静默跳过：用量只是统计口径，
// 不该因为它让子任务失败。
func TestMergeUsageWithoutSessionScope(t *testing.T) {
	a, _, _ := newCheckpointAgent(t)
	child := &Session{ID: "subagent-t1"}
	child.AddUsage(llm.Usage{InputTokens: 10})
	NewSubagentRunner(a).mergeUsage(context.Background(), child) // 不得 panic
}

func TestValidateSubagentTasksLimitAndModes(t *testing.T) {
	tasks := make([]tools.SubagentTask, MaxSubagents+1)
	for i := range tasks {
		tasks[i] = tools.SubagentTask{ID: string(rune('a' + i)), Prompt: "探索", Mode: "explore", Paths: []string{"pkg/part" + string(rune('a'+i))}}
	}
	if err := validateSubagentTasks(tasks); err == nil {
		t.Fatal("超过最大数量应拒绝")
	}
	if err := validateSubagentTasks([]tools.SubagentTask{{ID: "x", Prompt: "实现", Mode: "implement"}}); err == nil {
		t.Fatal("implement 未声明 paths 应拒绝")
	}
}

func TestValidateSubagentTasksRejectsOverlap(t *testing.T) {
	tasks := []tools.SubagentTask{
		{ID: "a", Prompt: "修改前端", Mode: "implement", Paths: []string{"web/dist"}},
		{ID: "b", Prompt: "修改输入框", Mode: "implement", Paths: []string{"web/dist/ui.js"}},
	}
	if err := validateSubagentTasks(tasks); err == nil {
		t.Fatal("父目录与子路径重叠应拒绝并行")
	}
}

func TestValidateSubagentTasksAllowsIndependentScopes(t *testing.T) {
	tasks := []tools.SubagentTask{
		{ID: "a", Prompt: "探索 agent", Mode: "explore", Paths: []string{"pkg/agent"}},
		{ID: "b", Prompt: "探索 server", Mode: "explore", Paths: []string{"pkg/server"}},
	}
	if err := validateSubagentTasks(tasks); err != nil {
		t.Fatalf("独立范围不应拒绝：%v", err)
	}
}

// TestSubagentToolSetWhitelist 白名单按模式过滤：explore 只读；implement 加写集；
// 任何模式都不得包含 shell、委派、插件、技能创建工具。
func TestSubagentToolSetWhitelist(t *testing.T) {
	explore := subagentToolSet("explore")
	for _, name := range []string{"read_file", "list_dir", "search_files", "save_memory"} {
		if !explore[name] {
			t.Errorf("explore 缺少只读工具 %s", name)
		}
	}
	for _, name := range []string{"write_file", "edit_file", "delete_file", "run_command", "delegate_subagents", "create_skill"} {
		if explore[name] {
			t.Errorf("explore 不得暴露 %s", name)
		}
	}
	impl := subagentToolSet("implement")
	for _, name := range []string{"write_file", "edit_file", "delete_file"} {
		if !impl[name] {
			t.Errorf("implement 缺少写入工具 %s", name)
		}
	}
	for _, name := range []string{"run_command", "delegate_subagents", "create_skill"} {
		if impl[name] {
			t.Errorf("implement 不得暴露 %s", name)
		}
	}
}

// TestScopesOverlapRules 路径重叠检测的边界：相同、父子、大小写都必须算重叠；
// 空列表（未声明 paths）与任何范围重叠（保守拒绝并行）；
// partA / partA2 是不同目录（/ 边界），不构成重叠。
func TestScopesOverlapRules(t *testing.T) {
	overlap := [][]string{
		{"web/dist", "web/dist/ui.js"},
		{"pkg/a", "pkg/a/nested/x.go"},
		{"pkg/a", "PKG/A"},
	}
	for _, pair := range overlap {
		if !scopesOverlap([]string{pair[0]}, []string{pair[1]}) {
			t.Errorf("%q 与 %q 应判定重叠", pair[0], pair[1])
		}
	}
	if !scopesOverlap(nil, []string{"anything"}) {
		t.Error("空列表（未声明 paths）与任何范围都应判定重叠（保守拒绝）")
	}
	if scopesOverlap([]string{"pkg/agent"}, []string{"pkg/server"}) {
		t.Error("独立目录不应判定重叠")
	}
	if scopesOverlap([]string{"partA"}, []string{"partA2"}) {
		t.Error("partA 与 partA2 是 / 边界分隔的不同目录，不应判定重叠")
	}
}

// TestTruncateTail 截尾保末：错误关键信息在末尾，截断必须保留结尾而非开头。
func TestTruncateTail(t *testing.T) {
	if got := truncateTail("short", 300); got != "short" {
		t.Errorf("短文本不应截断: %q", got)
	}
	long := strings.Repeat("a", 400) + "ERROR-HERE"
	got := truncateTail(long, 300)
	if len(got) != 3+300 || !strings.HasSuffix(got, "ERROR-HERE") {
		t.Errorf("截断必须保结尾: len=%d suffix-ok=%v", len(got), strings.HasSuffix(got, "ERROR-HERE"))
	}
	if truncateTail("  \n ok \t", 10) != "ok" {
		t.Error("先 TrimSpace 再截断")
	}
}
