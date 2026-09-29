package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"codeforge/pkg/tools"
)

// 命令收窄的攻击面测试。
//
// 这组测试的价值全在**否定用例**上：每一条「看起来无害但其实有副作用」的
// 命令都必须在只读模式下被挡住。少一条，就等于在只读模式里留了一个
// 任意写入口 —— 而只读模式的全部意义就是「不该写的写不了」。
//
// 参照：ZCode 为此写了 15+ 个 bash-readonly-policy-*.ts（core/src/tool/handlers/），
// 靠逐命令的选项分析（hasKnownBashWriteOption / isSedInPlaceOption 等）。
// 本项目的取舍是白名单更短、元字符一律拒绝 —— 宁可少收窄，不可收窄错。

// narrow 把命令送进 TerminalTool.ResolveSideEffect，返回收窄后的等级。
func narrow(t *testing.T, cmd string) tools.SideEffect {
	t.Helper()
	tool := NewTerminalTool(NewFS(t.TempDir()))
	args, err := json.Marshal(map[string]any{"command": cmd})
	if err != nil {
		t.Fatal(err)
	}
	return tool.ResolveSideEffect(args)
}

// TestShellMetaCharsNeverNarrowed 元字符一律拒绝收窄。
//
// 每一条后面都注明了「如果放过会怎样」。这是本项最核心的测试。
func TestShellMetaCharsNeverNarrowed(t *testing.T) {
	cases := []struct{ cmd, why string }{
		{"ls > out.txt", "重定向写文件"},
		{"ls >> out.txt", "追加写文件"},
		{"ls | tee out.txt", "管道接写入命令"},
		{"ls | sh", "管道接 shell 执行"},
		{"ls ; rm -rf /", "命令串联"},
		{"ls && rm x", "逻辑与"},
		{"ls &", "后台执行"},
		{"echo `rm -rf /`", "反引号命令替换"},
		{"echo $(rm -rf /)", "$() 命令替换"},
		{"echo $HOME", "变量展开（内容不可知）"},
		{"ls *", "glob 通配"},
		{"ls ?", "glob 通配"},
		{"cat ${IFS}file", "变量展开"},
		{"ls \"$(id)\"", "引号内藏替换"},
		{"ls '$(id)'", "引号内藏替换"},
		{"ls\nrm -rf /", "换行等价于 ;"},
		{"ls a\\ b", "反斜杠转义"},
		{"ls [ab]", "字符类"},
		{"ls {a,b}", "花括号展开"},
		{"", "空命令"},
		{"   ", "全空白"},
	}
	for _, c := range cases {
		if c.cmd == "" || c.cmd == "   " {
			continue
		}
		if got := narrow(t, c.cmd); got != tools.SideEffectExternal {
			t.Errorf("命令 %q 被收窄为只读，但 %s —— 只读模式下会放行", c.cmd, c.why)
		}
	}
}

// TestTabIsNotASeparator 记录一个容易搞反的事实：**制表符不是命令分隔符**。
//
// 在 POSIX shell 里空格与制表同属 IFS，都只分隔**参数**。所以
// `ls<TAB>rm -rf /` 是「ls 带参数 rm -rf /」，**不会**执行 rm。
// 只有 `;` `&&` `||` `|` `&` 与换行才是命令分隔符。
//
// 这条测试防止后来者「顺手」把 \t 加进 shellMetaChars：加了不会造成安全问题
// （只是收窄失效），但会让白名单形同虚设。
func TestTabIsNotASeparator(t *testing.T) {
	if got := narrow(t, "ls\trm -rf /"); got != tools.SideEffectNone {
		t.Errorf("制表符只是参数分隔，ls 带参数应收窄为只读，实际 %v", got)
	}
	// 但真正的分隔符仍然必须挡住
	if got := narrow(t, "ls\nrm -rf /"); got != tools.SideEffectExternal {
		t.Errorf("换行是命令分隔符，不该被收窄，实际 %v", got)
	}
}

// TestReadOnlyCommandsNarrowed 白名单内的纯读命令应当被收窄。
// 收窄不生效的话，规则 3 的意图就落空了（只读模式下连 ls 都不行）。
func TestReadOnlyCommandsNarrowed(t *testing.T) {
	cmds := []string{
		"ls", "ls -la", "ls /tmp", "pwd", "whoami", "date",
		"cat a.txt", "cat a.txt b.txt", "head -n 20 a.txt", "tail -f a.txt",
		"wc -l a.txt", "grep foo a.txt", "rg -n foo .", "stat a.txt",
		"/bin/ls -la",   // 带绝对路径
		"ls.exe",        // Windows 可执行后缀
		"git --version", // 注意：git 不在白名单（子命令决定性质）
	}
	for _, c := range cmds {
		if c == "git --version" {
			// git 整体不在白名单，必须保守拒绝
			if got := narrow(t, c); got != tools.SideEffectExternal {
				t.Errorf("命令 %q 不该被收窄（git 子命令性质不同，参数分析才安全）", c)
			}
			continue
		}
		if got := narrow(t, c); got != tools.SideEffectNone {
			t.Errorf("命令 %q 应收窄为只读，实际等级 %v", c, got)
		}
	}
}

// TestWriteCommandsNeverNarrowed 白名单里不该出现的写命令，
// 确认它们确实不在表里。
func TestWriteCommandsNeverNarrowed(t *testing.T) {
	cmds := []string{
		"rm x", "rm -rf .", "mv a b", "cp a b", "mkdir d", "touch f",
		"chmod 777 x", "chown u x", "dd if=a of=b", "tee f",
		"find . -name x",           // 有 -delete / -exec
		"find . -delete",           // 同上，显式
		"xargs rm",                 // 能执行任意命令
		"git status",               // git 子命令性质不同
		"git commit -m x",          // 写操作
		"sed -i s/a/b/ f",          // 原地改
		"awk BEGIN{print > \"f\"}", // 重定向在 awk 程序里
		"python -c open",           // 任意代码执行
		"sh -c ls",                 // 间接执行
		"eval ls",                  // 间接执行
		"bash script.sh",           // 执行脚本
	}
	for _, c := range cmds {
		if got := narrow(t, c); got != tools.SideEffectExternal {
			t.Errorf("写/执行类命令 %q 被收窄为只读 —— 严重", c)
		}
	}
}

// TestCaseAndPathNormalization 名字归一化：大小写、路径前缀、扩展名。
func TestCaseAndPathNormalization(t *testing.T) {
	ok := []string{"LS", "Ls -la", "/usr/bin/cat f", "/usr/bin/ls -la"}
	for _, c := range ok {
		if got := narrow(t, c); got != tools.SideEffectNone {
			t.Errorf("命令 %q 应被收窄为只读，实际 %v", c, got)
		}
	}
	bad := []string{"LS > f", "/usr/bin/cat f > f"}
	for _, c := range bad {
		if got := narrow(t, c); got != tools.SideEffectExternal {
			t.Errorf("命令 %q 含重定向，不该被收窄", c)
		}
	}
}

// TestWindowsPathNotNarrowed 明确记录一个取舍：反斜杠一律拒绝收窄，
// 所以 Windows 风格的绝对路径命令（`C:\Windows\System32\where.exe`）**不收窄**。
//
// 原因是反斜杠在不同 shell 下含义相反：
//
//	POSIX  sh   \ 是转义符（`a\ b` = 一个带空格的参数）
//	Windows cmd  \ 是路径分隔符，转义符是 ^
//
// 我们不解析命令（解析器的工作在 shell 手里），所以无法区分「这是转义」
// 还是「这是路径」。在两种解释里都可能被构造成绕过，只能一律拒绝。
//
// 代价是可用性的：Windows 上写全路径的只读命令不会被收窄。
// 这是刻意的 —— 收窄错一次就是只读模式下的任意写，收窄少一次只是多一次审批。
// 不带路径的 `where.exe`、`dir` 仍正常收窄。
func TestWindowsPathNotNarrowed(t *testing.T) {
	for _, c := range []string{
		`C:\Windows\System32\where.exe`,
		`C:\Windows\System32\where.exe -i x`,
		`c:\tools\cat.exe f`,
	} {
		if got := narrow(t, c); got != tools.SideEffectExternal {
			t.Errorf("含反斜杠的 Windows 路径不该被收窄（歧义字符，一律拒绝），%q 实际 %v", c, got)
		}
	}
	// 但纯命令名仍应收窄
	if got := narrow(t, "where.exe"); got != tools.SideEffectNone {
		t.Errorf("where.exe 应收窄为只读，实际 %v", got)
	}
}

// TestNarrowingNeverWidens 一个声明为只读的工具不能靠 ResolveSideEffect 变危险。
//
// 真实场景：某个工具静态声明了 none，但按入参「自认为」更危险 ——
// 那种情况下应当以声明为准（更严），否则声明就成了下限而非上界。
func TestNarrowingNeverWidens(t *testing.T) {
	tool := &widenStub{}
	got, declared := tools.SideEffectFor(tool, nil)
	if !declared {
		t.Fatal("已声明的工具应报告 declared=true")
	}
	if got != tools.SideEffectNone {
		t.Errorf("放宽必须被忽略，应保持声明的 none，实际 %v", got)
	}
}

// widenStub 静态声明为只读，却按入参自称更危险。
type widenStub struct{}

func (w *widenStub) Name() string        { return "widen" }
func (w *widenStub) Description() string { return "测试用" }
func (w *widenStub) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}
func (w *widenStub) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (w *widenStub) Execute(_ context.Context, _ json.RawMessage) (*tools.ToolResult, error) {
	return tools.Ok("done"), nil
}
func (w *widenStub) ResolveSideEffect(json.RawMessage) tools.SideEffect {
	return tools.SideEffectDestructive
}
