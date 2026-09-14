package agent

import (
	"fmt"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// 策略归一化：越界一律收敛到硬上限，绝不因为配置写错而放大并发。
func TestSubagentPolicyNormalize(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, MaxSubagents},
		{-1, MaxSubagents},
		{-100, MaxSubagents},
		{MaxSubagents + 1, MaxSubagents},
		{999, MaxSubagents},
		{MaxSubagents, MaxSubagents},
		{1, 1},
		{2, 2},
	}
	for _, c := range cases {
		p := SubagentPolicy{MaxConcurrent: c.in, AllowWrite: true, AllowDelete: true, AllowMemory: true}
		if got := p.Normalize().MaxConcurrent; got != c.want {
			t.Errorf("Normalize(MaxConcurrent=%d) 期望 %d，实际 %d", c.in, c.want, got)
		}
	}
}

// 默认策略必须与历史行为完全一致：5 并发、可写可删可记忆。
func TestDefaultSubagentPolicyMatchesHistory(t *testing.T) {
	p := DefaultSubagentPolicy()
	if p.MaxConcurrent != MaxSubagents {
		t.Errorf("默认并发期望 %d，实际 %d", MaxSubagents, p.MaxConcurrent)
	}
	if !p.AllowWrite || !p.AllowDelete || !p.AllowMemory {
		t.Errorf("默认策略应三项能力全开，实际 %+v", p)
	}
}

// MaxSubagents 必须与 config 的硬上限同源（否则设置页滑条与实际校验会不一致）。
func TestMaxSubagentsSharesCapWithConfig(t *testing.T) {
	if MaxSubagents != config.SubagentConcurrencyCap {
		t.Fatalf("MaxSubagents=%d 与 config.SubagentConcurrencyCap=%d 不同源",
			MaxSubagents, config.SubagentConcurrencyCap)
	}
}

// NewSubagentPolicy 从配置派生：能力项透传，并发项走归一化。
func TestNewSubagentPolicyFromConfig(t *testing.T) {
	no, yes := false, true
	cfg := config.Default()
	cfg.Subagents.MaxConcurrent = 2
	cfg.Subagents.AllowWrite = &no
	cfg.Subagents.AllowDelete = &no
	cfg.Subagents.AllowMemory = &yes

	p := NewSubagentPolicy(*cfg)
	if p.MaxConcurrent != 2 {
		t.Errorf("并发期望 2，实际 %d", p.MaxConcurrent)
	}
	if p.AllowWrite || p.AllowDelete {
		t.Errorf("禁写禁删未透传，实际 %+v", p)
	}
	if !p.AllowMemory {
		t.Errorf("允许记忆未透传，实际 %+v", p)
	}

	// 缺省配置（三指针为 nil）→ 全开 + 硬上限。
	d := NewSubagentPolicy(*config.Default())
	if !d.AllowWrite || !d.AllowDelete || !d.AllowMemory || d.MaxConcurrent != MaxSubagents {
		t.Errorf("缺省配置派生结果不符：%+v", d)
	}

	// 越界并发被收敛。
	bad := *config.Default()
	bad.Subagents.MaxConcurrent = 99
	if got := NewSubagentPolicy(bad).MaxConcurrent; got != MaxSubagents {
		t.Errorf("越界并发期望收敛到 %d，实际 %d", MaxSubagents, got)
	}
}

// 工具白名单：工具集只能随策略做减法，永远不含 shell / 插件 / 递归委派。
func TestSubagentToolSetPolicySubtractsOnly(t *testing.T) {
	full := DefaultSubagentPolicy()
	base := subagentToolSetPolicy("explore", full)
	for _, banned := range []string{
		"run_command", "delegate_subagents", "create_skill", "write_file", "edit_file", "delete_file",
	} {
		if base[banned] {
			t.Errorf("explore 白名单不得包含 %q", banned)
		}
	}
	for _, must := range []string{"read_file", "list_dir", "search_files", "save_memory"} {
		if !base[must] {
			t.Errorf("explore 白名单应包含 %q", must)
		}
	}

	impl := subagentToolSetPolicy("implement", full)
	for _, must := range []string{
		"read_file", "list_dir", "search_files", "save_memory", "write_file", "edit_file", "delete_file",
	} {
		if !impl[must] {
			t.Errorf("implement（全开策略）白名单应包含 %q", must)
		}
	}
	if impl["run_command"] || impl["delegate_subagents"] {
		t.Error("implement 白名单绝不含 shell / 递归委派")
	}

	// 禁写：implement 退化为只读（不再有写/编辑/删除）。
	noWrite := full
	noWrite.AllowWrite = false
	got := subagentToolSetPolicy("implement", noWrite)
	for _, banned := range []string{"write_file", "edit_file", "delete_file"} {
		if got[banned] {
			t.Errorf("禁写策略下 implement 不得包含 %q", banned)
		}
	}
	for _, must := range []string{"read_file", "list_dir", "search_files"} {
		if !got[must] {
			t.Errorf("禁写策略下仍应保留只读工具 %q", must)
		}
	}

	// 禁删：写/编辑保留，删除消失。
	noDelete := full
	noDelete.AllowDelete = false
	got = subagentToolSetPolicy("implement", noDelete)
	if !got["write_file"] || !got["edit_file"] {
		t.Error("仅禁删时不应影响写/编辑")
	}
	if got["delete_file"] {
		t.Error("禁删时不应暴露 delete_file")
	}

	// 禁记忆：仅 save_memory 消失。
	noMem := full
	noMem.AllowMemory = false
	got = subagentToolSetPolicy("implement", noMem)
	if got["save_memory"] {
		t.Error("禁记忆时不应暴露 save_memory")
	}
	if !got["read_file"] || !got["write_file"] {
		t.Error("禁记忆不应影响文件工具")
	}
}

// 策略「只收不放」：任何组合都必须严格是默认策略集合的子集。
func TestSubagentToolSetPolicyIsSubsetOfDefault(t *testing.T) {
	all := []bool{true, false}
	for _, w := range all {
		for _, d := range all {
			for _, m := range all {
				p := SubagentPolicy{MaxConcurrent: 3, AllowWrite: w, AllowDelete: d, AllowMemory: m}
				for _, mode := range []string{"explore", "implement"} {
					got := subagentToolSetPolicy(mode, p)
					def := subagentToolSetPolicy(mode, DefaultSubagentPolicy())
					for name := range got {
						if !def[name] {
							t.Errorf("策略 %+v 在 %s 模式放出了默认集合之外的工具 %q", p, mode, name)
						}
					}
				}
			}
		}
	}
}

// 兼容签名：subagentToolSet 必须等价于「默认策略」版本（既有测试与调用方依赖它）。
func TestSubagentToolSetEqualsDefaultPolicy(t *testing.T) {
	for _, mode := range []string{"explore", "implement"} {
		legacy := subagentToolSet(mode)
		byPolicy := subagentToolSetPolicy(mode, DefaultSubagentPolicy())
		if len(legacy) != len(byPolicy) {
			t.Fatalf("%s：subagentToolSet 与默认策略集合大小不同（%d vs %d）", mode, len(legacy), len(byPolicy))
		}
		for k, v := range legacy {
			if byPolicy[k] != v {
				t.Errorf("%s：工具 %q 在两个实现间不一致", mode, k)
			}
		}
	}
}

// 提示词措辞必须与实际授予的工具集一致，否则模型会反复调用不存在的写工具而空转。
func TestSubagentPromptMatchesGrantedTools(t *testing.T) {
	full := DefaultSubagentPolicy()

	// 1) explore：无论策略如何，都明确「只读」。
	task := tools.SubagentTask{ID: "e", Prompt: "看看 X", Mode: "explore", Paths: []string{"a"}}
	if p := subagentPrompt(task, full); !strings.Contains(p, "只读") {
		t.Errorf("explore 提示词未声明只读：%s", p)
	}

	// 2) implement + 禁写：不能说「你是实现子智能体」，且要给出改动清单的要求。
	noWrite := full
	noWrite.AllowWrite = false
	impl := tools.SubagentTask{ID: "i", Prompt: "改 Y", Mode: "implement", Paths: []string{"b"}}
	p := subagentPrompt(impl, noWrite)
	if !strings.Contains(p, "只读") {
		t.Errorf("禁写的 implement 提示词未声明只读：%s", p)
	}
	if strings.Contains(p, "实现子智能体") {
		t.Errorf("禁写时不得自称实现子智能体（措辞与工具集不符）：%s", p)
	}
	if !strings.Contains(p, "改动清单") {
		t.Errorf("禁写时应要求输出改动清单：%s", p)
	}

	// 3) implement + 禁删：仍是想写就写，但必须提示不要删除。
	noDelete := full
	noDelete.AllowDelete = false
	p = subagentPrompt(impl, noDelete)
	if !strings.Contains(p, "实现子智能体") {
		t.Errorf("可写时提示词应称实现子智能体：%s", p)
	}
	if !strings.Contains(p, "不要删除文件") {
		t.Errorf("禁删时提示词应明确禁止删除：%s", p)
	}
	if !strings.Contains(p, "由主智能体执行") {
		t.Errorf("禁删时提示词应说明删除由主智能体代劳：%s", p)
	}

	// 4) 全开：声明范围拼进提示词，且带上「不越界」的约束。
	p = subagentPrompt(impl, full)
	if !strings.Contains(p, "b") {
		t.Errorf("提示词应包含声明路径：%s", p)
	}
	if !strings.Contains(p, "不要调用其它子智能体") {
		t.Errorf("提示词应禁止递归委派：%s", p)
	}
}

// 并发上限可调：调小后 validateSubagentTasksMax 立即生效，调小不得放大。
func TestValidateSubagentTasksMaxHonorsPolicy(t *testing.T) {
	// 每个探索任务给一个互不重叠的目录，避免撞上「范围重叠」这条无关校验。
	tasks := func(n int) []tools.SubagentTask {
		out := make([]tools.SubagentTask, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, tools.SubagentTask{
				ID:     string(rune('a' + i)),
				Prompt: "p",
				Mode:   "explore",
				Paths:  []string{fmt.Sprintf("dir%d", i)},
			})
		}
		return out
	}

	if err := validateSubagentTasksMax(tasks(2), 2); err != nil {
		t.Errorf("2 个任务 / 上限 2 应通过，实际 %v", err)
	}
	if err := validateSubagentTasksMax(tasks(3), 2); err == nil {
		t.Error("3 个任务 / 上限 2 应被拒绝")
	}
	// 上限自身越界 → 收敛到硬上限（而非拒绝一切）。
	if err := validateSubagentTasksMax(tasks(3), 99); err != nil {
		t.Errorf("上限越界时应收敛到 %d 而非报错，实际 %v", MaxSubagents, err)
	}
	if err := validateSubagentTasksMax(tasks(MaxSubagents), 0); err != nil {
		t.Errorf("上限为 0 时应收敛到 %d，实际 %v", MaxSubagents, err)
	}
}
