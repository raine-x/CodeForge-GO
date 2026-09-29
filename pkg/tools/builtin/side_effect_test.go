package builtin

import (
	"testing"

	"codeforge/config"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// 2.3 的回归护栏：**每个内置工具都必须声明副作用等级。**
//
// 为什么这个测试比「一张名字表」可靠：名字表是数据，会忘记同步；
// 声明是类型，新增工具时漏写会直接编译不过 —— 加上这个测试，
// 漏声明的路径就只剩「CI 变红」一种。
//
// 装配方式刻意与 cmd/agent/main.go 保持一致（同一组 Register* 函数），
// 所以这里数到的就是生产环境真正会发给模型的那批工具。

type memStoreStub struct{}

func (memStoreStub) AddMemory(string) (int64, error) { return 0, nil }

type todoStoreStub struct{}

func (todoStoreStub) SaveTodos(string, []store.TodoRow) error { return nil }

func allBuiltin(reg *tools.Registry, t *testing.T) {
	t.Helper()
	fs := NewFS(t.TempDir())
	RegisterFS(reg, fs)
	RegisterTerminal(reg, fs)
	RegisterSearch(reg, fs)
	RegisterMemory(reg, memStoreStub{})
	RegisterTodo(reg, todoStoreStub{})
	RegisterWeb(reg, config.WebConfig{})
	RegisterSkillCreator(reg, fs)
}

// TestAllBuiltinToolsDeclareSideEffect 逐个断言声明存在且取值合法。
func TestAllBuiltinToolsDeclareSideEffect(t *testing.T) {
	reg := tools.NewRegistry()
	allBuiltin(reg, t)

	list := reg.List()
	if len(list) < 10 {
		t.Fatalf("内置工具只有 %d 个，装配方式可能已失效，测试本身失去意义", len(list))
	}
	for _, tl := range list {
		meta, ok := tools.MetadataOf(tl)
		if !ok {
			t.Errorf("内置工具 %s 未声明（或声明非法）副作用等级 —— "+
				"只读模式会把它当「非只读」拒绝，而这个漏项不会有任何报错", tl.Name())
			continue
		}
		// 等级与工具名必须自洽：名字看着是只读的，声明却不许是写操作。
		switch tl.Name() {
		case "read_file", "list_dir", "search_files", "find_files":
			if meta.SideEffect != tools.SideEffectNone {
				t.Errorf("工具 %s 是只读的，声明却是 %v", tl.Name(), meta.SideEffect)
			}
		case "write_file", "edit_file":
			if meta.SideEffect != tools.SideEffectWrite {
				t.Errorf("工具 %s 应声明为 write，实为 %v", tl.Name(), meta.SideEffect)
			}
		case "delete_file":
			if meta.SideEffect != tools.SideEffectDestructive {
				t.Errorf("delete_file 应声明为 destructive（不可撤销），实为 %v", meta.SideEffect)
			}
		case "run_command":
			if meta.SideEffect != tools.SideEffectExternal {
				t.Errorf("run_command 应声明为 external（可起进程/碰网络），实为 %v", meta.SideEffect)
			}
		}
	}
}

// TestReadOnlyModeSetIsNonEmpty 防止「全都没声明」这种空转状态。
// 全部 fail-closed 会让只读模式什么都做不了，测试会假装通过。
func TestReadOnlyModeSetIsNonEmpty(t *testing.T) {
	reg := tools.NewRegistry()
	allBuiltin(reg, t)

	var readonly []string
	for _, tl := range reg.List() {
		if meta, ok := tools.MetadataOf(tl); ok && meta.SideEffect == tools.SideEffectNone {
			readonly = append(readonly, tl.Name())
		}
	}
	if len(readonly) < 4 {
		t.Fatalf("声明为只读的内置工具只有 %d 个（%v），只读模式将不可用",
			len(readonly), readonly)
	}
}
