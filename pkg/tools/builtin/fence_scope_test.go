package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/security"
	"codeforge/pkg/tools"
)

// fenceApprover 是固定答复的审批器（true=批准，false=拒绝）。
type fenceApprover struct{ approved bool }

func (a fenceApprover) RequestApproval(_ context.Context, _ tools.ApprovalRequest) (bool, error) {
	return a.approved, nil
}

// newFenceFixture 造「工作区 a + 区外工作区 b」并注册全部路径类工具。
func newFenceFixture(t *testing.T) (reg *tools.Registry, rootA, rootB string) {
	t.Helper()
	base := t.TempDir()
	rootA = filepath.Join(base, "a") // 用户选中的工作区 a
	rootB = filepath.Join(base, "b") // 未选中的工作区 b
	for _, d := range []string{rootA, rootB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(rootB, "secret.txt"), []byte("TOPSECRET-B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootA, "inside.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := NewFS(rootA)
	reg = tools.NewRegistry()
	RegisterFS(reg, fs)
	RegisterSearch(reg, fs)
	RegisterTerminal(reg, fs)
	return reg, rootA, rootB
}

// newFenceExecutor 按指定权限模式构建执行器。
func newFenceExecutor(t *testing.T, reg *tools.Registry, mode string, approver tools.Approver) *tools.Executor {
	t.Helper()
	al, err := security.NewAuditLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })
	pol := security.NewPolicy(config.SecurityConfig{
		DefaultDecision:     "ask",
		AutoApproveReadOnly: true,
		PermissionMode:      mode,
		Rules: []config.SecurityRule{
			{Tools: []string{"read_file", "list_dir", "search_files"}, Decision: "allow"},
			{Tools: []string{"write_file", "edit_file", "delete_file", "run_command"}, Decision: "ask"},
		},
	})
	return tools.NewExecutor(reg, pol, al, approver, 30_000_000_000, 32*1024)
}

func runTool(t *testing.T, exec *tools.Executor, name string, args any) *tools.ToolResult {
	t.Helper()
	raw := mustJSON(t, args)
	res, err := exec.Execute(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("%s Execute 返回 Go error: %v", name, err)
	}
	return res
}

// TestCommandTouchesOutside 命令文本越界扫描的启发式用例。
func TestCommandTouchesOutside(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	fs := NewFS(root)
	outside := filepath.Join(base, "b")

	cases := []struct {
		cmd string
		out bool
	}{
		{"ls -la", false},
		{"git status && git log --oneline -5", false},
		{"go test ./pkg/... -run TestX", false},
		{"echo hello > out.txt", false},
		{"grep -r TODO .", false},
		{"curl -s https://example.com/api -o resp.json", false},
		{"cat ../b/secret.txt", true},
		{"cat ..\\b\\secret.txt", true},
		{"type C:\\Windows\\win.ini", true},
		{"cat " + filepath.Join(outside, "secret.txt"), true},
		{"cat ../b/secret.txt; rm inside.txt", true},
		{"cd .. && cat b/secret.txt", true},
		{"cat ../b/dir/file.txt > local.txt", true},
		{"echo x > ../b/pwned.txt", true},
		{"echo x >> ../b/pwned.txt", true},
		{`cat "../b/secret.txt"`, true},
		{"cat ~/secrets.txt", true},
		{"cat $HOME/secrets.txt", true},
		{"type %APPDATA%\\secrets.txt", true},
		{"cat /c/Users/other/secret.txt", true}, // MSYS 根路径
		{"cp inside.txt /c/other/", true},
		{"find .. -name '*.log'", true},
		{"2>/dev/null make build", false}, // /dev/null 短开关形态放行，不误伤
	}
	for _, c := range cases {
		if got := fs.commandTouchesOutside(c.cmd); got != c.out {
			t.Errorf("commandTouchesOutside(%q) = %v, 期望 %v", c.cmd, got, c.out)
		}
	}
}

// TestAutoModeOutsideRequiresApproval 自主模式下全部自动通过：越界不弹审批、
// 直接执行（黑名单与工作区内部围栏仍兜底）。
func TestAutoModeOutsideRequiresApproval(t *testing.T) {
	reg, _, _ := newFenceFixture(t)

	// 无审批通道也直接放行：自主模式不请求审批
	exec := newFenceExecutor(t, reg, security.ModeAuto, nil)
	res := runTool(t, exec, "read_file", map[string]string{"path": "../b/secret.txt"})
	if !res.Success || !strings.Contains(showRes(res), "TOPSECRET") {
		t.Fatalf("auto 模式越界 read_file 应自动放行，实际: %v", showRes(res))
	}

	// 判定层面：越界也是 allow，不产生 ask
	if d := exec.Evaluate("run_command", mustJSON(t, map[string]string{"command": "cat ../b/secret.txt"})); d.Decision != security.Allow {
		t.Fatalf("auto 模式越界判定应为 allow，实际 %s（%s）", d.Decision, d.Reason)
	}
	// 区内命令不受影响，仍自动放行
	if d := exec.Evaluate("run_command", mustJSON(t, map[string]string{"command": "echo hi"})); d.Decision != security.Allow {
		t.Fatalf("auto 模式区内命令应放行，实际 %s（%s）", d.Decision, d.Reason)
	}
}

// TestAskModeOutsideRequiresApproval 请求模式下越界同样走审批（既有行为保持）。
func TestAskModeOutsideRequiresApproval(t *testing.T) {
	reg, _, _ := newFenceFixture(t)
	exec := newFenceExecutor(t, reg, security.ModeAsk, nil)

	if d := exec.Evaluate("read_file", mustJSON(t, map[string]string{"path": "../b/secret.txt"})); d.Decision != security.Ask {
		t.Fatalf("ask 模式越界 read_file 判定应为 ask，实际 %s", d.Decision)
	}
	if d := exec.Evaluate("run_command", mustJSON(t, map[string]string{"command": "cat ../b/secret.txt"})); d.Decision != security.Ask {
		t.Fatalf("ask 模式越界 run_command 判定应为 ask，实际 %s", d.Decision)
	}
	// 区内只读仍然放行
	if d := exec.Evaluate("read_file", mustJSON(t, map[string]string{"path": "inside.txt"})); d.Decision != security.Allow {
		t.Fatalf("ask 模式区内只读应放行，实际 %s", d.Decision)
	}
}

// TestReadOnlyModeOutsideFence 只读模式：只允许探索与搜索等读取操作 ——
// 区外只读工具放行（不弹审批），区外写/命令维持拒绝。
func TestReadOnlyModeOutsideFence(t *testing.T) {
	reg, _, _ := newFenceFixture(t)
	exec := newFenceExecutor(t, reg, security.ModeReadOnly, fenceApprover{approved: true})

	// 越界只读工具（read_file）：放行，不再要求审批
	if d := exec.Evaluate("read_file", mustJSON(t, map[string]string{"path": "../b/secret.txt"})); d.Decision != security.Allow {
		t.Fatalf("readonly 模式越界 read_file 应放行（只读工具），实际 %s（%s）", d.Decision, d.Reason)
	}
	res := runTool(t, exec, "read_file", map[string]string{"path": "../b/secret.txt"})
	if !res.Success || !strings.Contains(showRes(res), "TOPSECRET") {
		t.Fatalf("readonly 模式越界只读工具应能读取，实际: %v", showRes(res))
	}
	// 越界命令（非只读）：维持拒绝
	if d := exec.Evaluate("run_command", mustJSON(t, map[string]string{"command": "cat ../b/secret.txt"})); d.Decision != security.Deny {
		t.Fatalf("readonly 模式越界 run_command 应维持 deny，实际 %s", d.Decision)
	}
	// 区内只读：照常自动放行
	res = runTool(t, exec, "read_file", map[string]string{"path": "inside.txt"})
	if !res.Success {
		t.Fatalf("readonly 模式区内只读应放行: %s", res.Error)
	}
}

// TestResolveCheckedSymlinkAncestor 区内软链指向区外且目标文件尚不存在时也必须拦截
// （堵住「只解析已存在路径」的写入绕过）。
func TestResolveCheckedSymlinkAncestor(t *testing.T) {
	fs, root, outside := newGuardFS(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境不支持创建软链接，跳过: %v", err)
	}
	target := filepath.Join("link", "newfile.txt") // link/newfile.txt 尚不存在
	if _, err := fs.ResolveChecked(target); err == nil {
		t.Fatal("经由区内软链向区外写入新文件的路径应被拒绝")
	}
	if _, err := os.Stat(filepath.Join(outside, "newfile.txt")); err == nil {
		t.Fatal("仅解析不应产生区外文件")
	}
}

// showRes 把结果压成可读字符串，便于断言内容泄露。
func showRes(res *tools.ToolResult) string {
	data, _ := json.Marshal(res.Data)
	return res.Error + " " + string(data)
}
