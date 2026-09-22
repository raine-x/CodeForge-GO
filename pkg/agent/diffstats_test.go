package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
)

// previewFor 对 edit_file/write_file 应同时给出 +/- 行数统计与 unified diff 文本；
// 非文件类工具（run_command）两者都不产生。
func TestDiffStatsFor(t *testing.T) {
	dir := t.TempDir()
	registry := tools.NewRegistry()
	fs := builtin.NewFS(dir)
	builtin.RegisterFS(registry, fs)

	ag := &Agent{registry: registry}

	// 准备一个 4 行的文件
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(map[string]any{
		"path":       "a.txt",
		"old_string": "l2\nl3\n",
		"new_string": "x\n",
	})
	diff, st := ag.previewFor(llm.ToolCall{Name: "edit_file", Input: input})
	if st == nil {
		t.Fatal("edit_file 应产生 DiffStats")
	}
	if st.Added != 1 || st.Removed != 2 {
		t.Errorf("期望 +1/-2，实际 +%d/-%d", st.Added, st.Removed)
	}
	// diff 文本必须一起给出来（前端「点击查看改动位置」靠它），且与统计同源。
	// 头部用的是**解析后的绝对路径**，所以这里只断言文件名与改动行。
	for _, want := range []string{"+x", "-l2", "-l3", "a.txt"} {
		if !strings.Contains(diff, want) {
			t.Errorf("previewFor 应给出 diff 文本，缺少 %q：\n%s", want, diff)
		}
	}

	// run_command 不是 DiffProvider → 两者皆空
	diff, st = ag.previewFor(llm.ToolCall{Name: "run_command", Input: []byte(`{"command":"go build"}`)})
	if st != nil || diff != "" {
		t.Errorf("run_command 不应有 DiffStats/diff，实际 %+v / %q", st, diff)
	}
}

// 大文件的单处小改动必须只报真正变动的行数。
//
// 2026-09-22 反馈：2524 行的 assets/www/index.html 改一行，界面显示 +2524/-2520
// （= 整份文件行数）。根因是 diff 的「整文件退化」阈值 4_000_000 = 2000²，
// 文件一超过 2000 行就走整文件替换。这里用 2520 行钉住回归。
func TestDiffStatsForLargeFile(t *testing.T) {
	dir := t.TempDir()
	registry := tools.NewRegistry()
	builtin.RegisterFS(registry, builtin.NewFS(dir))
	ag := &Agent{registry: registry}

	const lines = 2520
	var sb strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&sb, "line %d: some stable content here\n", i)
	}
	target := filepath.Join(dir, "index.html")
	if err := os.WriteFile(target, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(map[string]any{
		"path":       "index.html",
		"old_string": "line 1500: some stable content here\n",
		"new_string": "line 1500: CHANGED content here\n",
	})
	st := previewStats(t, ag, llm.ToolCall{Name: "edit_file", Input: input})
	if st.Added != 1 || st.Removed != 1 {
		t.Errorf("%d 行文件改 1 行应报 +1/-1，实际 +%d/-%d", lines, st.Added, st.Removed)
	}
}

// previewStats 只取统计部分，顺带断言 diff 文本非空（两者必须同源产出）。
func previewStats(t *testing.T, ag *Agent, tc llm.ToolCall) *DiffStats {
	t.Helper()
	diff, st := ag.previewFor(tc)
	if st == nil {
		t.Fatalf("%s 应产生 DiffStats", tc.Name)
	}
	if diff == "" {
		t.Fatalf("%s 应同时产生 diff 文本", tc.Name)
	}
	return st
}
