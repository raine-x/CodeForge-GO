package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"codeforge/pkg/llm"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
)

// diffStatsFor 对 edit_file/write_file 应返回正确的 +/- 行数统计；
// 非文件类工具（run_command）无 diff，返回 nil。
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
	st := ag.diffStatsFor(llm.ToolCall{Name: "edit_file", Input: input})
	if st == nil {
		t.Fatal("edit_file 应产生 DiffStats")
	}
	if st.Added != 1 || st.Removed != 2 {
		t.Errorf("期望 +1/-2，实际 +%d/-%d", st.Added, st.Removed)
	}

	// run_command 不是 DiffProvider → nil
	st = ag.diffStatsFor(llm.ToolCall{Name: "run_command", Input: []byte(`{"command":"go build"}`)})
	if st != nil {
		t.Errorf("run_command 不应有 DiffStats，实际 %+v", st)
	}
}
