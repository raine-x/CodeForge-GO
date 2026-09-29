package tools

import (
	"context"
	"encoding/json"
	"testing"

	"codeforge/config"
	"codeforge/pkg/security"
)

// 2.3 剩余部分：按**入参**收窄副作用等级。
//
// 现状：run_command 一律声明 SideEffectExternal，于是
//   - 只读权限模式下连 `ls` 都被拒（可用性问题）
//   - ask 模式下 `ls` 也要弹审批（审批疲劳）
//
// 收窄能解决这两点，但有一个必须先想清楚的危险：
// **命令里的 `>` `|` `;` 是 shell 解释的，不是我们解释的。**
// 只看首个 token 的话，`echo hi > important.txt` 会被判成「echo 是只读」，
// 于是一个写操作在只读模式下畅通无阻 —— 这是个能直接造成数据损坏的漏洞。
//
// 所以收窄必须满足两个条件，缺一不可：
//  1. 首个命令词在只读白名单里；
//  2. 整条命令**不含任何**可能产生副作用的 shell 元字符。
//
// 参照：ZCode 的 resolvePermissionCapability（executor/permission-capability.ts:6-22）
// 与 OpenCode 的同名机制。关键不变量与它们一致：**只能收窄，不能放宽**。

// resolverStub 声明为 base，但按入参收窄为 narrow。
type resolverStub struct {
	sideEffectStub
	base   SideEffect
	narrow SideEffect
	// returnHigher 为 true 时故意返回一个**更高**的等级（应被忽略）
	returnHigher bool
}

func (r *resolverStub) Metadata() Metadata { return Metadata{SideEffect: r.base} }

func (r *resolverStub) ResolveSideEffect(json.RawMessage) SideEffect {
	if r.returnHigher {
		return SideEffectDestructive
	}
	return r.narrow
}

func newExec(t *testing.T, mode string, tools ...Tool) *Executor {
	t.Helper()
	reg := NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode:      mode,
		DefaultDecision:     "allow",
		AutoApproveReadOnly: true,
	})
	return NewExecutor(reg, pol, nil, nil, 0, 0)
}

// TestSideEffectCanOnlyNarrow 核心不变量：收窄只能降级，放宽必须被忽略。
//
// 若这条不成立，一个写工具就能靠「按入参返回 none」骗过只读模式。
func TestSideEffectCanOnlyNarrow(t *testing.T) {
	e := newExec(t, security.ModeReadOnly, &resolverStub{
		sideEffectStub: sideEffectStub{name: "evil"},
		base:           SideEffectWrite,
		narrow:         SideEffectNone,
	})
	if got, _ := SideEffectFor(&resolverStub{
		sideEffectStub: sideEffectStub{name: "x"},
		base:           SideEffectWrite,
		narrow:         SideEffectNone,
	}, nil); got != SideEffectNone {
		t.Errorf("降级应收窄成功，实际 %v", got)
	}
	// 声明为 none 的工具试图「放宽」到 destructive，必须被忽略
	got, _ := SideEffectFor(&resolverStub{
		sideEffectStub: sideEffectStub{name: "x"},
		base:           SideEffectNone,
		returnHigher:   true,
	}, nil)
	if got != SideEffectNone {
		t.Errorf("放宽必须被忽略，实际 %v", got)
	}
	_ = e
}

// TestNarrowingAppliesInReadOnlyMode 收窄后只读模式应放行。
func TestNarrowingAppliesInReadOnlyMode(t *testing.T) {
	tool := &resolverStub{
		sideEffectStub: sideEffectStub{name: "probe"},
		base:           SideEffectExternal,
		narrow:         SideEffectNone,
	}
	e := newExec(t, security.ModeReadOnly, tool)
	res, _ := e.Execute(context.Background(), "probe", json.RawMessage(`{}`))
	if !res.Success {
		t.Errorf("收窄为只读后应放行: %+v", res)
	}
}

// TestNoNarrowingStaysBlocked 未收窄的工具在只读模式下仍被拒（回归护栏）。
func TestNoNarrowingStaysBlocked(t *testing.T) {
	tool := &resolverStub{
		sideEffectStub: sideEffectStub{name: "probe"},
		base:           SideEffectExternal,
		narrow:         SideEffectExternal, // 收窄成自己 = 不收窄
	}
	e := newExec(t, security.ModeReadOnly, tool)
	res, _ := e.Execute(context.Background(), "probe", json.RawMessage(`{}`))
	if res.Success {
		t.Errorf("未收窄的工具在只读模式下不该放行")
	}
}
