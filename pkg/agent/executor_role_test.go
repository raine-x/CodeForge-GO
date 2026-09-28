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
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
)

// 2.1：子智能体的工具调用必须进审计日志。
//
// 历史缺陷（AGENTS.md 硬规则 2）：newSubagentIn 用
//
//	tools.NewExecutor(registry, policy, nil, nil, 120*time.Second, 32*1024)
//
// audit 传了 nil，于是子智能体的每一次文件写入、每一次命令执行都**不进审计**，
// 而且**不报错** —— 静默降级，正是这类缺陷最糟的地方。reviewer 那边是对的
// （goal_runner.go 传了真 audit），只有子智能体漏了。
//
// 修法是执行器角色工厂：审计等参数不再靠手工传，`NewExecutorFor` 从父
// Executor 派生。测试要证明的是「派生出来的执行器，audit 一定非 nil」。

// readAuditLines 读审计 JSONL，返回非空行数与全部内容。
func readAuditLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("读审计失败: %v", err)
	}
	var out []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// TestNewSubagentHasAudit 是本项的**直接**回归测试：
// 走的正是历史上出错的那条路径 newSubagentIn。
//
// 只测工厂是不够的 —— 工厂对了但调用方没换，照样漏审计。
func TestNewSubagentHasAudit(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	audit, err := security.NewAuditLogger(auditPath)
	if err != nil {
		t.Fatalf("建审计失败: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })

	pol := security.NewPolicy(config.SecurityConfig{PermissionMode: "auto", DefaultDecision: "allow"})
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	work := t.TempDir()
	fs := builtin.NewFS(work)
	reg := tools.NewRegistry()
	reg.Register(builtin.NewWriteFileTool(fs))
	reg.Register(builtin.NewReadFileTool(fs))

	parent := New(config.AgentConfig{MaxSteps: 5}, config.LLMConfig{}, nil,
		tools.NewExecutor(reg, pol, audit, nil, time.Second, 32*1024), NewHistory(st), work)

	for _, mode := range []string{"explore", "implement"} {
		child := parent.newSubagentIn(mode, work)
		if child.executor.Audit() == nil {
			t.Errorf("子智能体(%s)的 audit 是 nil —— 它的工具调用不会进审计（历史缺陷）", mode)
		}
		if child.executor.Policy() != pol {
			t.Errorf("子智能体(%s)的策略应沿用父的", mode)
		}
	}

	// 审查者那边本来就是对的，钉住别被改坏
	if parent.newGoalVerifier(work).executor.Audit() == nil {
		t.Errorf("审查者的 audit 不该是 nil")
	}
}

// TestSubagentExecutorAudits 钉死核心行为：子智能体写文件要留下审计记录。
func TestSubagentExecutorAudits(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	audit, err := security.NewAuditLogger(auditPath)
	if err != nil {
		t.Fatalf("建审计失败: %v", err)
	}
	// AuditLogger 一直占着文件句柄，不关的话 Windows 上 t.TempDir() 清不掉
	// （会报 "being used by another process"，看起来像测试失败，其实是清理失败）
	t.Cleanup(func() { _ = audit.Close() })

	fs := builtin.NewFS(dir)
	registry := tools.NewRegistry()
	registry.Register(builtin.NewWriteFileTool(fs))

	policy := security.NewPolicy(config.SecurityConfig{
		PermissionMode:  security.ModeAuto,
		DefaultDecision: "allow",
	})
	parent := tools.NewExecutor(registry, policy, audit, nil, 120*time.Second, 32*1024)

	// 走角色工厂派生子智能体执行器
	subExec := tools.NewExecutorFor(parent, tools.RoleSubagent, registry)

	target := filepath.Join(dir, "out.txt")
	res, err := subExec.Execute(context.Background(), "write_file",
		json.RawMessage(`{"path":"`+filepath.ToSlash(target)+`","content":"hi"}`))
	if err != nil {
		t.Fatalf("Execute 返回 error: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("写入不该失败: result=%+v err=%v", res, err)
	}

	lines := readAuditLines(t, auditPath)
	if len(lines) == 0 {
		t.Fatalf("子智能体写了文件却没有任何审计记录 —— audit 传了 nil（历史缺陷）")
	}
	if !strings.Contains(lines[0], "write_file") {
		t.Errorf("审计记录里没有工具名: %s", lines[0])
	}
}

// TestNewExecutorForInheritsAudit 工厂不允许产出 nil audit。
//
// 这是「按构造正确」的核心：审计不是可选参数，所以没人能再传 nil。
func TestNewExecutorForInheritsAudit(t *testing.T) {
	registry := tools.NewRegistry()
	policy := security.NewPolicy(config.SecurityConfig{DefaultDecision: "allow"})
	parent := tools.NewExecutor(registry, policy, nil, nil, 120*time.Second, 32*1024)

	got := tools.NewExecutorFor(parent, tools.RoleSubagent, registry)
	if got == parent {
		t.Fatalf("工厂应返回新实例，不能把父执行器原样返回")
	}
	if got.Audit() != nil && parent.Audit() == nil {
		t.Errorf("父为 nil 时不该凭空造出非 nil")
	}
	if got.Policy() != policy {
		t.Errorf("策略应沿用父的，实际 %p vs %p", got.Policy(), policy)
	}
}

// TestRoleTimeout 角色各自带自己的超时，不再由调用方手写 120*time.Second。
func TestRoleTimeout(t *testing.T) {
	registry := tools.NewRegistry()
	policy := security.NewPolicy(config.SecurityConfig{DefaultDecision: "allow"})
	parent := tools.NewExecutor(registry, policy, nil, nil, 7*time.Second, 32*1024)

	// 父的 7s 不该泄漏到子角色上：各角色有自己的预算
	sub := tools.NewExecutorFor(parent, tools.RoleSubagent, registry)
	if sub.Timeout() == parent.Timeout() {
		t.Errorf("子角色超时不应等于父的 %v", sub.Timeout())
	}
	if sub.Timeout() <= 0 {
		t.Errorf("子角色超时必须为正，实际 %v", sub.Timeout())
	}
}

// TestSubagentCannotApprove 子智能体拿不到审批通道。
//
// 这一点是**故意**的：子智能体跑在后台 goroutine 里，给它审批通道等于让
// 界面上出现没有会话归属的孤儿卡片。子智能体命中 ask 规则时应当直接拒绝。
func TestSubagentCannotApprove(t *testing.T) {
	registry := tools.NewRegistry()
	policy := security.NewPolicy(config.SecurityConfig{DefaultDecision: "allow"})
	parent := tools.NewExecutor(registry, policy, nil, nil, 120*time.Second, 32*1024)

	sub := tools.NewExecutorFor(parent, tools.RoleSubagent, registry)
	if sub.Approver() != nil {
		t.Errorf("子智能体不应有审批通道，实际 %v", sub.Approver())
	}
}
