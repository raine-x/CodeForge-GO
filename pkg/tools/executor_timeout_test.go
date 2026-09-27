package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/security"
)

// timeoutStub 是一个按固定值申请超时的工具。
type timeoutStub struct {
	name string
	d    time.Duration
	// block 让 Execute 睡一会儿，用来观察是否真被超时砍断
	block time.Duration
}

func (s *timeoutStub) Name() string        { return s.name }
func (s *timeoutStub) Description() string { return "超时策略测试桩" }
func (s *timeoutStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (s *timeoutStub) ToolTimeout(json.RawMessage) time.Duration { return s.d }
func (s *timeoutStub) Execute(ctx context.Context, _ json.RawMessage) (*ToolResult, error) {
	if s.block <= 0 {
		return Ok("done"), nil
	}
	select {
	case <-time.After(s.block):
		return Ok("done"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// plainStub 不实现 TimeoutPolicy。
type plainStub struct{ name string }

func (s *plainStub) Name() string        { return s.name }
func (s *plainStub) Description() string { return "普通工具桩" }
func (s *plainStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (s *plainStub) Execute(context.Context, json.RawMessage) (*ToolResult, error) {
	return Ok("done"), nil
}

func newTimeoutExecutor(t *testing.T, timeout time.Duration) *Executor {
	t.Helper()
	reg := NewRegistry()
	// ⚠️ policy 不能传 nil：Execute 里 decide() 会解引用它，nil 直接 panic。
	// 用 auto 模式 + 默认放行，本组只关心超时、不想被审批缠上。
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode:  "auto",
		DefaultDecision: "allow",
	})
	return NewExecutor(reg, pol, nil, nil, timeout, 32*1024)
}

func TestTimeoutFor(t *testing.T) {
	const def = 120 * time.Second

	t.Run("不实现 TimeoutPolicy → 用默认", func(t *testing.T) {
		e := newTimeoutExecutor(t, def)
		if got := e.timeoutFor(&plainStub{name: "read_file"}, nil); got != def {
			t.Errorf("应沿用默认 %v，实际 %v", def, got)
		}
	})

	t.Run("申请更长 → 采纳", func(t *testing.T) {
		e := newTimeoutExecutor(t, def)
		tool := &timeoutStub{name: "goal_verify", d: 30 * time.Minute}
		if got := e.timeoutFor(tool, nil); got != 30*time.Minute {
			t.Errorf("应采纳 30m，实际 %v", got)
		}
	})

	t.Run("返回 0 / 负数 → 回落默认（绝不能是 0）", func(t *testing.T) {
		// context.WithTimeout(0) 是**立即**超时：一次误判会让工具秒失败，
		// 报出来的是「工具执行超时或被取消」，比真超时难查得多。
		e := newTimeoutExecutor(t, def)
		for _, d := range []time.Duration{0, -time.Second} {
			tool := &timeoutStub{name: "goal_verify", d: d}
			got := e.timeoutFor(tool, nil)
			if got != def {
				t.Errorf("申请 %v 时应回落默认 %v，实际 %v", d, def, got)
			}
		}
	})

	t.Run("申请更短 → 不采纳（只允许放宽）", func(t *testing.T) {
		// 收紧留给工具自己（轮数/步数）。若这里允许收紧，
		// 「默认 120s 防失控」就会被单个工具悄悄拆掉。
		e := newTimeoutExecutor(t, def)
		tool := &timeoutStub{name: "goal_verify", d: time.Second}
		if got := e.timeoutFor(tool, nil); got != def {
			t.Errorf("收紧应被忽略、仍为 %v，实际 %v", def, got)
		}
	})
}

// TestTimeoutPolicyActuallyWidens 是行为断言：只测 timeoutFor 等于自证自明，
// 这里真的跑一次执行 —— 睡眠超过默认值但在申请值之内，必须成功返回。
func TestTimeoutPolicyActuallyWidens(t *testing.T) {
	e := newTimeoutExecutor(t, 200*time.Millisecond)
	tool := &timeoutStub{name: "goal_verify", d: 3 * time.Second, block: 700 * time.Millisecond}
	e.Registry().Register(tool)

	// ⚠️ Execute 按本仓库约定**永远返回 nil error**：业务失败装在 ToolResult 里
	// （见 Execute 里 err != nil 分支的注释）。所以只判 res.Success。
	res, _ := e.Execute(context.Background(), "goal_verify", json.RawMessage(`{}`))
	if res == nil {
		t.Fatalf("不应返回 nil 结果")
	}
	if !res.Success {
		t.Errorf("申请了更长超时后应执行完成，实际: %+v", res)
	}
}

// 反向：没申请更长（或申请无效值）的工具，超时照样砍断 —— 兜底没被削弱。
func TestTimeoutStillApplies(t *testing.T) {
	t.Run("未申请", func(t *testing.T) {
		e := newTimeoutExecutor(t, 150*time.Millisecond)
		e.Registry().Register(&timeoutStub{name: "slow", d: 0, block: 3 * time.Second})
		res, _ := e.Execute(context.Background(), "slow", json.RawMessage(`{}`))
		if res != nil && res.Success {
			t.Errorf("超过默认超时（且未申请放宽）应被砍断，实际成功返回")
		}
	})

	t.Run("申请了但比默认短 → 仍按默认砍断", func(t *testing.T) {
		e := newTimeoutExecutor(t, 150*time.Millisecond)
		e.Registry().Register(&timeoutStub{name: "shrink", d: 20 * time.Millisecond, block: 3 * time.Second})
		res, _ := e.Execute(context.Background(), "shrink", json.RawMessage(`{}`))
		if res != nil && res.Success {
			t.Errorf("收紧申请应被忽略、仍按默认超时砍断，实际成功返回")
		}
	})
}
