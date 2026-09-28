package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/security"
)

// 这组测试回答一个具体问题：**人工审批的等待上界来自哪里？**
//
// 曾经的错误结论（已由本文件推翻）：
//
//	「审批复用工具执行超时（默认 120s），所以点同意晚了会被当成拒绝。」
//
// 代码事实是：Execute 在 executor.go:185 用的是**自己的入参 ctx**，
// 而那个 120s 的 cctx 是在 executor.go:273 的 run() 里才创建的，
// run() 的调用点在 executor.go:216 —— **在审批之后**。
// 也就是说审批完全不受工具超时约束。
//
// 那它到底受什么约束？答案是**只有调用方传入的 ctx**。而生产链路上：
//
//	agent.go:1282  a.executor.Execute(toolCtx, ...)
//	agent.go:1272  toolCtx := tools.WithSession(ctx, ...)   // 只挂值，不加超时
//	                （整个 agent.go 里没有任何 WithTimeout/WithCancel）
//	ws_handler.go:311 startUserMessage → runLoopWithLimit(ctx, ...)
//
// 即：**审批的实际上界是 WebSocket 连接的生命周期**。
// 用户把审批卡片挂着不处理，这个 goroutine 就一直等 —— 没有独立超时。
//
// 正确结论：审批**需要一个自己的超时**，且这个超时应当短于连接生命周期，
// 否则「连接还在、审批已无人问津」这种情况会永久占用执行器。
// 超时后必须**明确报错**而不是静默当作拒绝（见文末断言）。

// slowApprover 按固定时长阻塞后给出审批结果，用来观测审批路径等待了多久。
type slowApprover struct {
	wait  time.Duration
	ok    bool
	calls atomic.Int32
}

func (a *slowApprover) RequestApproval(ctx context.Context, _ ApprovalRequest) (bool, error) {
	a.calls.Add(1)
	select {
	case <-time.After(a.wait):
		return a.ok, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// newAskExecutor 造一个「某工具必须人工审批」的执行器。
// mode 必须是 ask：policy.go:171 在 ModeAuto 下会直接 Allow，
// 第 3 步的插件强制审批（RequireApproval）根本走不到。
func newAskExecutor(t *testing.T, toolName string, toolTimeout, defaultTimeout time.Duration) (*Executor, *slowApprover) {
	t.Helper()
	reg := NewRegistry()
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode:  security.ModeAsk,
		DefaultDecision: "allow",
	})
	pol.RequireApproval(toolName)
	reg.Register(&timeoutStub{name: toolName, d: toolTimeout})

	ap := &slowApprover{wait: 200 * time.Millisecond, ok: true}
	return NewExecutor(reg, pol, nil, ap, defaultTimeout, 32*1024), ap
}

// TestApprovalNotBoundedByToolTimeout 钉死核心事实：
// 工具超时 40ms，人工审批要 200ms —— 如果审批复用工具超时，
// 这次调用会在 40ms 就失败。它成功了，且确实等了 200ms 以上。
func TestApprovalNotBoundedByToolTimeout(t *testing.T) {
	const (
		toolTimeout = 40 * time.Millisecond
		approvalLat = 200 * time.Millisecond
	)
	e, ap := newAskExecutor(t, "write_file", toolTimeout, 120*time.Second)

	start := time.Now()
	// ⚠️ Execute 按本仓库约定**永远返回 nil error**：业务失败装在 ToolResult 里。
	res, _ := e.Execute(context.Background(), "write_file", json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if ap.calls.Load() != 1 {
		t.Fatalf("审批应被调用 1 次，实际 %d 次", ap.calls.Load())
	}
	if !res.Success {
		t.Errorf("审批通过后应成功返回，实际失败: %+v", res)
	}
	if elapsed < approvalLat {
		t.Errorf("审批实际只等了 %v（工具超时是 %v）—— 说明审批被工具超时砍断了，"+
			"文档里的旧结论成立", elapsed, toolTimeout)
	}
	if elapsed >= 120*time.Second {
		t.Fatalf("不该发生：等待超过了默认工具超时 120s，实际 %v", elapsed)
	}
}

// TestApprovalBoundedByCallerContext 证明审批的**唯一**上界是调用方 ctx。
// 这既是现状说明，也指出了修法：给审批单独一个 context.WithTimeout。
func TestApprovalBoundedByCallerContext(t *testing.T) {
	e, ap := newAskExecutor(t, "write_file", 40*time.Millisecond, 120*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, _ := e.Execute(ctx, "write_file", json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if ap.calls.Load() != 1 {
		t.Fatalf("审批应被调用 1 次，实际 %d 次", ap.calls.Load())
	}
	if res.Success {
		t.Errorf("审批未完成就被 ctx 取消，不应成功返回: %+v", res)
	}
	if elapsed > 150*time.Millisecond {
		t.Errorf("应被调用方 ctx（60ms）及时打断，实际用了 %v", elapsed)
	}
}

// TestApprovalHasIndependentTimeout 是修好之后的正向断言：
// 审批有自己的上界，且远短于工具超时语义。
//
// 修法：Executor 用 context.WithTimeout 给审批单独包一层 ctx。
func TestApprovalHasIndependentTimeout(t *testing.T) {
	const approvalTimeout = 150 * time.Millisecond
	e, ap := newAskExecutor(t, "write_file", 40*time.Millisecond, 120*time.Second)
	// 审批要等 5 秒，远超 approvalTimeout。
	// 注意改的是 newAskExecutor 返回的那个实例（e.approver 指向它），
	// 另建一个替换掉 e.approver 的话，ap.calls 统计的就是旧对象了。
	ap.wait = 5 * time.Second
	e.approvalTimeout = approvalTimeout

	// 无 deadline 的 ctx —— 与生产链路 agent.go:1282 的形态一致
	start := time.Now()
	res, _ := e.Execute(context.Background(), "write_file", json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if ap.calls.Load() != 1 {
		t.Fatalf("审批应被调用 1 次，实际 %d 次", ap.calls.Load())
	}
	if res.Success {
		t.Fatalf("审批超时不该成功返回: %+v", res)
	}
	if elapsed > 3*approvalTimeout {
		t.Errorf("审批应在 %v 处返回，实际 %v —— 没有独立上界", approvalTimeout, elapsed)
	}
	// 超时必须**明确报错**，不能静默当成「用户拒绝」——
	// 否则界面上看不出「操作没做」是因为人没点，还是因为等太久。
	msg := res.Error
	if strings.Contains(msg, "rejected") || strings.Contains(msg, "拒绝") {
		t.Errorf("超时被当成了拒绝，用户无法区分: %q", msg)
	}
	if !strings.Contains(msg, "超时") && !strings.Contains(msg, "timeout") {
		t.Errorf("错误信息应点明是审批超时，实际: %q", msg)
	}
}

// TestApprovalCallerCtxWinsWhenSooner 调用方 ctx 比审批超时更短时，听调用方的。
// 两个上界不能互相覆盖 —— 生产里会话取消（用户关页面）必须立即生效。
func TestApprovalCallerCtxWinsWhenSooner(t *testing.T) {
	e, _ := newAskExecutor(t, "write_file", 40*time.Millisecond, 120*time.Second)
	e.approver = &slowApprover{wait: 5 * time.Second, ok: true}
	e.approvalTimeout = 10 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	res, _ := e.Execute(ctx, "write_file", json.RawMessage(`{}`))
	elapsed := time.Since(start)

	if res.Success {
		t.Fatalf("不该成功: %+v", res)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("调用方 ctx（100ms）应立即生效，实际 %v", elapsed)
	}
}

// TestApprovalDefaultTimeoutSane 默认值必须存在，否则「独立超时」形同虚设。
func TestApprovalDefaultTimeoutSane(t *testing.T) {
	e, _ := newAskExecutor(t, "write_file", 40*time.Millisecond, 120*time.Second)
	if e.approvalTimeout <= 0 {
		t.Errorf("审批超时必须有正默认值，实际 %v", e.approvalTimeout)
	}
	// 上界要短于「连接一直挂着」这种无主场景，但不能短到正常思考时间
	if e.approvalTimeout < time.Minute {
		t.Errorf("默认审批超时 %v 过短，会误杀正常的用户思考时间", e.approvalTimeout)
	}
	if e.approvalTimeout > 2*time.Hour {
		t.Errorf("默认审批超时 %v 过长，卡住的审批会长期占用执行器", e.approvalTimeout)
	}
}
