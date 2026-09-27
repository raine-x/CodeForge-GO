package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/security"
)

// 这些用例锁住 2026-09-27 审计发现的两件事：
//
//  ① escalate 早先写成 `if d != Allow { return }`，而默认配置（permission_mode=ask
//     + write_file/edit_file/delete_file/run_command/web_* 都在 ask 规则里）下这些
//     工具**先**拿到 Ask → ScopeChecker 从未被问 → 越界审批不披露、且批准后
//     ScopeApproved 会顺带关掉工具内部的 checkScope。
//  ② 命中规则时 reason 里带工具代号（"命中规则：write_file"），会漏进审批弹窗的
//     悬停提示 —— 违反项目自己的「界面不出现内部工具 ID」纪律。

// scopeStub 是一个必定「越界」的可控工具。
type scopeStub struct {
	name     string
	outside  bool
	readOnly bool
	gotCtx   context.Context
}

func (s *scopeStub) Name() string        { return s.name }
func (s *scopeStub) Description() string { return "测试用" }
func (s *scopeStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (s *scopeStub) Execute(ctx context.Context, _ json.RawMessage) (*ToolResult, error) {
	s.gotCtx = ctx
	return Ok("done"), nil
}
func (s *scopeStub) OutsideScope(json.RawMessage) bool { return s.outside }
func (s *scopeStub) IsReadOnly() bool                  { return s.readOnly }

// recordApprover 记录审批请求并按预设答案回应。
type recordApprover struct {
	req     ApprovalRequest
	approve bool
	calls   int
}

func (a *recordApprover) RequestApproval(_ context.Context, req ApprovalRequest) (bool, error) {
	a.calls++
	a.req = req
	return a.approve, nil
}

func newScopeExecutor(t *testing.T, tool Tool, mode string, appr Approver) *Executor {
	t.Helper()
	reg := NewRegistry()
	reg.Register(tool)
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode:      mode,
		DefaultDecision:     "ask",
		AutoApproveReadOnly: true,
		Rules: []config.SecurityRule{
			{Tools: []string{tool.Name()}, Decision: "ask"},
		},
	})
	// 审计写临时文件，避免污染真实用户目录
	audit, err := security.NewAuditLogger(t.TempDir() + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	return NewExecutor(reg, pol, audit, appr, 2*time.Second, 4096)
}

// TestEscalateDisclosesOutOfScopeOnRuleAsk 是本次修复的核心断言。
//
// 越界 + 命中 ask 规则 → 判定仍是 Ask，但 reason **必须**含越界说明。
// 修复前 reason 只有「命中规则：write_file」，用户看不出这一步要跳出工作区。
func TestEscalateDisclosesOutOfScopeOnRuleAsk(t *testing.T) {
	for _, name := range []string{"write_file", "edit_file", "delete_file", "run_command", "web_fetch"} {
		t.Run(name, func(t *testing.T) {
			tool := &scopeStub{name: name, outside: true}
			appr := &recordApprover{approve: false}
			ex := newScopeExecutor(t, tool, "ask", appr)

			_, err := ex.Execute(context.Background(), name, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if appr.calls != 1 {
				t.Fatalf("应当弹一次审批，实际 %d 次", appr.calls)
			}
			if !strings.Contains(appr.req.Reason, "越出工作区") {
				t.Errorf("越界审批的 reason 必须披露「越出工作区」，实际 %q", appr.req.Reason)
			}
			// 原规则原因不能丢：用户既要知道「为什么需要审批」，也要知道「它要跳出去」
			if !strings.Contains(appr.req.Reason, "安全规则") {
				t.Errorf("应保留原规则原因，实际 %q", appr.req.Reason)
			}
		})
	}
}

// TestEscalateNoScopeNoteWhenInside 区内调用不该被加上越界说明（否则满屏都是噪音）。
func TestEscalateNoScopeNoteWhenInside(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: false}
	appr := &recordApprover{approve: false}
	ex := newScopeExecutor(t, tool, "ask", appr)

	if _, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if appr.calls != 1 {
		t.Fatalf("区内调用也应走审批（规则是 ask），实际 %d 次", appr.calls)
	}
	if strings.Contains(appr.req.Reason, "越出工作区") {
		t.Errorf("区内调用不该带越界说明，实际 %q", appr.req.Reason)
	}
}

// TestScopeApprovedOnlyForOutOfScope 收紧围栏豁免：区内调用被批准后**不得**注入豁免。
//
// 修复前任何被批准的 Ask 都注入 ScopeApproved，于是一次「看起来很常规」的区内
// 审批就把工作区围栏关掉了。
func TestScopeApprovedOnlyForOutOfScope(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: false}
	appr := &recordApprover{approve: true}
	ex := newScopeExecutor(t, tool, "ask", appr)

	if _, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if appr.calls != 1 {
		t.Fatalf("应弹一次审批，实际 %d", appr.calls)
	}
	if ScopeApprovedFrom(tool.gotCtx) {
		t.Error("区内调用被批准后不得注入围栏豁免（那等于白设工作区边界）")
	}
}

// TestScopeApprovedForApprovedOutOfScope 反向：越界 + 批准 → 豁免照旧，
// 否则收紧过头会把合法的越界写入也拦掉。
func TestScopeApprovedForApprovedOutOfScope(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: true}
	appr := &recordApprover{approve: true}
	ex := newScopeExecutor(t, tool, "ask", appr)

	res, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("批准的越界调用应执行成功，实际 %+v", res)
	}
	if !ScopeApprovedFrom(tool.gotCtx) {
		t.Error("越界调用被批准后应注入围栏豁免")
	}
}

// TestScopeApprovedNotInjectedOnReject 拒绝的越界调用不该留下豁免。
func TestScopeApprovedNotInjectedOnReject(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: true}
	appr := &recordApprover{approve: false}
	ex := newScopeExecutor(t, tool, "ask", appr)

	if _, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if tool.gotCtx != nil {
		t.Error("被拒绝的调用不应执行到工具")
	}
}

// TestDenyBeatsOutOfScopeDisclosure 黑名单优先于越界披露，审批不能解锁。
func TestDenyBeatsOutOfScopeDisclosure(t *testing.T) {
	reg := NewRegistry()
	tool := &scopeStub{name: "run_command", outside: true}
	reg.Register(tool)
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode: "ask",
		DenyPatterns:   []string{`rm\s+-rf\s+/`},
	})
	audit, err := security.NewAuditLogger(t.TempDir() + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = audit.Close() })
	appr := &recordApprover{approve: true}
	ex := NewExecutor(reg, pol, audit, appr, 2*time.Second, 4096)

	res, err := ex.Execute(context.Background(), "run_command", json.RawMessage(`{"command":"rm -rf /"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Success {
		t.Fatalf("命中黑名单必须 Deny，实际 %+v", res)
	}
	if appr.calls != 0 {
		t.Error("Deny 不得弹审批（审批不能解锁黑名单）")
	}
}

// TestAutoModeStillAllowsOutOfScope auto 模式不弹审批、照旧豁免围栏（既有行为不能破）。
func TestAutoModeStillAllowsOutOfScope(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: true}
	appr := &recordApprover{approve: false}
	ex := newScopeExecutor(t, tool, "auto", appr)

	res, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("auto 模式应自动放行，实际 %+v", res)
	}
	if appr.calls != 0 {
		t.Error("auto 模式不应弹审批")
	}
	if !ScopeApprovedFrom(tool.gotCtx) {
		t.Error("auto 模式放行越界时仍需注入围栏豁免")
	}
}

// TestReadOnlyModeRejectsNonReadOnlyOutside 只读模式下非只读工具的越界仍是 Deny。
func TestReadOnlyModeRejectsNonReadOnlyOutside(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: true, readOnly: false}
	ex := newScopeExecutor(t, tool, "readonly", &recordApprover{approve: true})

	res, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Success {
		t.Fatalf("只读模式禁止非只读工具越界，实际 %+v", res)
	}
}

// TestApprovalCarriesSubagentIdentity 审批要带上子任务身份。
//
// 一次委派最多并行 5 个子智能体，它们的审批会**同时**挂在界面上，而中文短语
// （都是「需要审批：写入文件」）与 session_id 都相同 —— 没有身份标识时，
// 用户批错那张照样授权了一次真实的、不同的写入。
func TestApprovalCarriesSubagentIdentity(t *testing.T) {
	t.Run("子智能体发起 → 带 subagent_id/mode", func(t *testing.T) {
		tool := &scopeStub{name: "write_file", outside: false}
		appr := &recordApprover{approve: false}
		ex := newScopeExecutor(t, tool, "ask", appr)
		ctx := WithSubagentScope(context.Background(), SubagentScope{
			Allowed: []string{"pkg/a.go"},
			Mode:    "implement",
			TaskID:  "task-1",
		})
		if _, err := ex.Execute(ctx, "write_file", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if appr.calls != 1 {
			t.Fatalf("应弹一次审批，实际 %d", appr.calls)
		}
		if appr.req.SubagentID != "task-1" || appr.req.SubagentMode != "implement" {
			t.Errorf("审批应带子任务身份，实际 id=%q mode=%q",
				appr.req.SubagentID, appr.req.SubagentMode)
		}
	})

	t.Run("explore 子任务（无 paths）也要带身份", func(t *testing.T) {
		// ⚠️ 这一条锁住「身份与围栏是两件事」：SubagentScopeFrom 的 ok 依赖
		// len(Allowed)>0，explore 子任务不必声明 paths，用它取身份会拿到 ok=false。
		tool := &scopeStub{name: "write_file", outside: false}
		appr := &recordApprover{approve: false}
		ex := newScopeExecutor(t, tool, "ask", appr)
		ctx := WithSubagentScope(context.Background(), SubagentScope{
			Mode: "explore", TaskID: "task-explore",
		})
		if _, err := ex.Execute(ctx, "write_file", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if appr.req.SubagentID != "task-explore" {
			t.Errorf("无 paths 的子任务也必须带身份，实际 %q", appr.req.SubagentID)
		}
	})

	t.Run("主智能体发起 → 不带（omitempty）", func(t *testing.T) {
		tool := &scopeStub{name: "write_file", outside: false}
		appr := &recordApprover{approve: false}
		ex := newScopeExecutor(t, tool, "ask", appr)
		if _, err := ex.Execute(context.Background(), "write_file", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if appr.req.SubagentID != "" {
			t.Errorf("主智能体的审批不该带 subagent_id，实际 %q", appr.req.SubagentID)
		}
	})
}

// 审批 reason 里的越界说明必须带进来（子智能体同样受围栏约束）
func TestSubagentOutOfScopeStillDisclosed(t *testing.T) {
	tool := &scopeStub{name: "write_file", outside: true}
	appr := &recordApprover{approve: false}
	ex := newScopeExecutor(t, tool, "ask", appr)
	ctx := WithSubagentScope(context.Background(), SubagentScope{
		Mode: "implement", TaskID: "t9",
	})
	if _, err := ex.Execute(ctx, "write_file", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(appr.req.Reason, "越出工作区") {
		t.Errorf("子智能体的越界审批同样要披露，实际 %q", appr.req.Reason)
	}
	if appr.req.SubagentID != "t9" {
		t.Errorf("身份应透传，实际 %q", appr.req.SubagentID)
	}
}

// TestApprovalReasonNeverLeaksToolID reason 会进审批弹窗的悬停提示，
// 绝不能出现内部工具代号（docs/hitl-approval-phrases.md 明确列为要避免的）。
func TestApprovalReasonNeverLeaksToolID(t *testing.T) {
	ids := []string{"write_file", "edit_file", "delete_file", "run_command",
		"read_file", "list_dir", "search_files", "web_fetch", "web_search"}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			for _, outside := range []bool{true, false} {
				tool := &scopeStub{name: id, outside: outside}
				appr := &recordApprover{approve: false}
				ex := newScopeExecutor(t, tool, "ask", appr)
				if _, err := ex.Execute(context.Background(), id, json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				if appr.calls == 0 {
					continue // 没弹审批就没有 reason 可查
				}
				if strings.Contains(appr.req.Reason, id) {
					t.Errorf("outside=%v：审批 reason 泄露了工具代号 %q → %q", outside, id, appr.req.Reason)
				}
			}
		})
	}
}
