package tools

import (
	"context"
	"encoding/json"
	"testing"

	"codeforge/config"
	"codeforge/pkg/security"
)

// 2.3：工具的副作用等级应当由工具**自己声明**，而不是靠一张按名字硬编码的表。
//
// 修复前的现状（两处真相来源，互相矛盾）：
//
//	a) security.readOnlyTools —— 硬编码表：read_file / list_dir / search_files /
//	   git_status / git_log
//	b) ReadOnlyTool 接口 —— 由工具自己实现 IsReadOnly()
//
// 两者不一致，且方向相反：
//
//   - find_files / web_fetch / web_search 真的只读、也真的实现了 IsReadOnly，
//     但**不在表里** → 只读模式下被误拒（可用性 bug）
//   - git_status / git_log 在表里，**却没有任何工具声明** → 光凭名字就放行。
//     而工具名是插件/MCP 可控的：一个叫 git_status 的插件工具在只读模式下
//     会被自动放行（权限提升）
//
// 所以本项不是「补一张表」，而是删掉表、让声明成为唯一来源。

// sideEffectStub 是一个完全受控的工具桩：名字可任意伪装，用来暴露
// 「按名字信任只读性」的漏洞。
type sideEffectStub struct {
	name string
	// meta 存在与否由是否实现 Metadata 决定，故用指针：nil = 未声明。
	meta *Metadata
}

func (s *sideEffectStub) Name() string        { return s.name }
func (s *sideEffectStub) Description() string { return "测试用" }
func (s *sideEffectStub) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (s *sideEffectStub) Execute(context.Context, json.RawMessage) (*ToolResult, error) {
	return Ok("done"), nil
}

// declStub 声明了副作用等级的工具桩（未声明的版本请用 sideEffectStub）。
type declStub struct{ sideEffectStub }

func (d *declStub) Metadata() Metadata { return *d.meta }

func newReadOnlyModeExecutor(t *testing.T, tools ...Tool) *Executor {
	t.Helper()
	reg := NewRegistry()
	for _, tl := range tools {
		reg.Register(tl)
	}
	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode:  security.ModeReadOnly,
		DefaultDecision: "allow",
	})
	return NewExecutor(reg, pol, nil, nil, 0, 0)
}

func runOnce(t *testing.T, e *Executor, name string) *ToolResult {
	t.Helper()
	res, err := e.Execute(context.Background(), name, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute 返回 error: %v", err)
	}
	return res
}

// TestReadOnlyModeTrustsDeclarationNotName 是本项的核心断言：
// 只读模式只认工具**自己**的声明，名字不算数。
func TestReadOnlyModeTrustsDeclarationNotName(t *testing.T) {
	e := newReadOnlyModeExecutor(t, &declStub{sideEffectStub{
		name: "git_status",
		// 名字命中旧硬编码表，但自己**声明为写操作** → 必须被拒
		meta: &Metadata{SideEffect: SideEffectWrite},
	}})
	res := runOnce(t, e, "git_status")
	if res.Success {
		t.Fatalf("冒充 git_status 的写工具在只读模式下被放行了 —— 按名字信任 = 插件可提权")
	}
}

// TestReadOnlyModeAllowsDeclaredReadOnly 反方向：
// 声明为只读的工具就该放行，不管名字在不在旧表里。
func TestReadOnlyModeAllowsDeclaredReadOnly(t *testing.T) {
	e := newReadOnlyModeExecutor(t, &declStub{sideEffectStub{
		name: "find_files", // 修复前：真只读但不在表里 → 被误拒
		meta: &Metadata{SideEffect: SideEffectNone},
	}})
	res := runOnce(t, e, "find_files")
	if !res.Success {
		t.Errorf("声明为只读的工具在只读模式下被拒了: %+v", res)
	}
}

// TestUndeclaredToolFailsClosed 未声明副作用的工具在只读模式下必须被拒。
//
// 关键安全取向：**没声明 = 不知道 = 不放行**。
// 旧设计里「漏实现 ReadOnlyTool」是静默降级（AGENTS.md 硬规则 3 点名的反模式）。
func TestUndeclaredToolFailsClosed(t *testing.T) {
	e := newReadOnlyModeExecutor(t, &sideEffectStub{name: "git_log"})
	res := runOnce(t, e, "git_log")
	if res.Success {
		t.Errorf("未声明副作用的工具在只读模式下被放行了 —— 应 fail-closed")
	}
}

// TestInvalidSideEffectIsTreatedAsUndeclared 声明了但取值非法 = 没声明。
func TestInvalidSideEffectIsTreatedAsUndeclared(t *testing.T) {
	e := newReadOnlyModeExecutor(t, &declStub{sideEffectStub{
		name: "weird",
		meta: &Metadata{SideEffect: SideEffect(42)},
	}})
	res := runOnce(t, e, "weird")
	if res.Success {
		t.Errorf("非法 SideEffect 取值被当成了合法声明 —— 应按未声明从严处理")
	}
}

// TestSideEffectLevelOrdering 副作用等级必须单调，便于写「不低于 X 即从严」这类规则。
func TestSideEffectLevelOrdering(t *testing.T) {
	order := []SideEffect{SideEffectNone, SideEffectWrite, SideEffectExternal, SideEffectDestructive}
	for i := 1; i < len(order); i++ {
		if order[i-1] >= order[i] {
			t.Errorf("等级未严格递增: %v >= %v", order[i-1], order[i])
		}
		if !order[i].Valid() {
			t.Errorf("%v 应为合法取值", order[i])
		}
	}
	if SideEffect(99).Valid() {
		t.Errorf("越界取值 99 不该被判为合法")
	}
	if !SideEffectNone.Valid() || SideEffect(0).Valid() != true {
		t.Errorf("SideEffectNone 应为合法")
	}
}
