package agent

import (
	"strings"
	"testing"

	"codeforge/pkg/tools"
)

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
