package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"codeforge/pkg/backend"
	"codeforge/pkg/errs"
	"codeforge/pkg/tools"
)

// itoaExit 把退出码转成字符串（文案用）。
func itoaExit(n int) string { return strconv.Itoa(n) }

// TerminalTool 跨平台异步执行 Shell 命令并收集输出。
type TerminalTool struct{ fs *FS }

// NewTerminalTool 构造 run_command 工具。
func NewTerminalTool(fs *FS) *TerminalTool { return &TerminalTool{fs: fs} }

// Metadata 声明副作用等级。
//
// run_command 的等级是 External 而非 Write：它能起进程、能碰网络、
// 写工作区之外的任何路径。用 Write 描述会低估它。
func (t *TerminalTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectExternal}
}

// ResolveSideEffect 按命令内容把等级**收窄**为只读。
//
// 为什么这条收窄是安全的 —— 也是为什么它必须这么保守：
//
// 命令字符串最终交给 **shell** 解释，而**我们不解释它**。所以只判断首个
// token 是不够的：`echo hi > important.txt` 的首个词是 `echo`（在白名单里），
// 但真正产生副作用的是那个 `>`，由 shell 处理。
//
// 因此收窄要求**两个条件同时成立**：
//
//  1. 首个命令词在只读白名单里；
//  2. 整条命令不含任何「可能产生副作用」的 shell 元字符。
//
// 第 2 条一旦放宽，`>` 重定向、`|` 管道、`;` 串联、“ ` “/`$()` 命令替换、
// `&` 后台、glob 通配都会从门缝里过去 —— 那等于在只读模式下开了一个
// 任意写入口。任何拿不准的情形一律不收窄。
func (t *TerminalTool) ResolveSideEffect(args json.RawMessage) tools.SideEffect {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return tools.SideEffectExternal
	}
	if isReadOnlyShellCommand(a.Command) {
		return tools.SideEffectNone
	}
	return tools.SideEffectExternal
}

// readOnlyCommands 是「命令词本身就只读」的白名单。
//
// 刻意短小：只收**不需要看参数就能确定只读**的命令。
// 像 `find`（有 -delete / -exec）、`git`（子命令决定）、`tee`、
// `xargs`（能执行任意命令）都不在列表里 —— 收窄它们需要真正的参数分析，
// 那是 ZCode 写了 15+ 个 bash-readonly-policy-*.ts 的原因，不该照抄。
//
// 反过来，`ls` / `cat` / `grep` / `wc` / `head` / `tail` / `pwd` 这一类
// 覆盖了只读模式下的绝大多数真实需求。
var readOnlyCommands = map[string]bool{
	"ls": true, "dir": true,
	"cat": true, "bat": true,
	"head": true, "tail": true,
	"wc": true, "stat": true, "file": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true,
	"pwd": true, "whoami": true, "hostname": true,
	"date": true, "uname": true, "id": true,
	"which": true, "whereis": true, "type": true, "where": true,
	"df": true, "du": true, "tree": true, "printenv": true,
	"echo": true,
}

// shellMetaChars 是「一旦出现就拒绝收窄」的字符集合。
//
// 逐个说明为什么危险：
//
//	>  >>   重定向写文件
//	|        管道可接写入命令（`ls | tee f`、`ls | sh`）
//	;        命令分隔，可接任意后续命令
//	&        后台 / 逻辑与
//	`  $( )  命令替换：`$(rm -rf x)` 的首个词看着无害
//	*  ?     glob：文件名可能被展开成任意内容；`rm *` 也是靠它
//	\  '  "  引号内可藏元字符，无法可靠判断
//	$        变量展开
//	{} [] ()  花括号 / 字符类 / 子 shell
//	\n \r     换行与回车等价于 ;
//
// **刻意不含空格与制表符**：它们只是参数分隔，`ls -la /tmp` 才是最常见的
// 只读命令，把它们列为元字符等于让收窄完全失效。
//
// 结论：宁可把 `ls -la` 收窄掉，也不能让 `ls > /etc/passwd` 过去。
var shellMetaChars = ">|<&;`$*?()[]{}\\'\"\n\r"

// isReadOnlyShellCommand 判断一条 shell 命令是否可安全收窄为只读。
func isReadOnlyShellCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	// 任何元字符 → 不收窄。
	if strings.ContainsAny(cmd, shellMetaChars) {
		return false
	}
	// 首个词就是命令本身（此时没有分隔符，Fields 的结果就是命令）。
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	// 命令可能带路径：`/bin/ls`、`C:\...\ls.exe`。
	word := fields[0]
	if i := strings.LastIndexAny(word, `/\`); i >= 0 {
		word = word[i+1:]
	}
	word = strings.TrimSuffix(strings.ToLower(word), ".exe")
	return readOnlyCommands[word]
}

// Name 实现 tools.Tool。
func (t *TerminalTool) Name() string { return "run_command" }

// Description 实现 tools.Tool。
func (t *TerminalTool) Description() string {
	return "在当前平台默认 Shell 中执行一条命令，返回合并后的标准输出/错误与退出码。属于危险操作，通常需要人工审批。"
}

// InputSchema 实现 tools.Tool。
func (t *TerminalTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("command", "要执行的 Shell 命令", true).
		Str("cwd", "工作目录（必须位于工作区内），留空使用工作区根目录", false).
		Int("timeout_sec", "超时秒数，默认 60", false).
		Build()
}

// OutsideScope 实现 tools.ScopeChecker：cwd 越界，或命令文本引用了工作区之外的
// 路径（绝对路径、.. 穿越、~ 主目录、cd 换目录逃逸、无法静态解析的 $ 变量路径等）。
func (t *TerminalTool) OutsideScope(args json.RawMessage) bool {
	var p struct {
		Command string `json:"command"`
		Cwd     string `json:"cwd"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return true // 参数不可解析时保守判定为需要审批
	}
	if t.fs.outsidePath(p.Cwd) {
		return true
	}
	return t.fs.commandTouchesOutside(p.Command)
}

// Execute 实现 tools.Tool。
func (t *TerminalTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Command    string `json:"command"`
		Cwd        string `json:"cwd"`
		TimeoutSec int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if strings.TrimSpace(p.Command) == "" {
		return tools.Err("command 不能为空"), nil
	}

	timeout := 60 * time.Second
	if p.TimeoutSec > 0 {
		timeout = time.Duration(p.TimeoutSec) * time.Second
	}

	dir, err := t.fs.ResolveCheckedCtx(ctx, p.Cwd)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("执行命令", err)), nil
	}

	// 执行委托给 Backend：进程组管理、WaitDelay、退出码判定全是**后端的义务**。
	// 留在工具层就等于假设「命令一定在本机跑」—— 远程后端接入时，
	// run_command 要么没法实现，要么把本地 shell 硬塞进远程场景。
	//
	// 工具层只保留：围栏校验、结果整形、以及「超时 / 启动失败」的文案区分。
	res, execErr := t.fs.backend().Exec(ctx, backend.ExecRequest{
		Command: p.Command,
		Dir:     dir,
		Timeout: timeout,
	})
	if execErr != nil {
		return tools.Err("执行命令失败: %v", execErr), nil
	}
	if res.StartErr != nil {
		return tools.Err("启动命令失败: %v", res.StartErr), nil
	}

	exitCode := res.ExitCode
	out := res.Output
	meta := map[string]any{
		"exit_code":   exitCode,
		"duration_ms": res.Duration.Milliseconds(),
		"shell":       res.Shell,
	}

	if res.TimedOut {
		return tools.OkMeta(map[string]any{
			"output":    out,
			"exit_code": exitCode,
			"note":      "命令执行超时，已被终止",
		}, meta), nil
	}
	if exitCode < 0 {
		return tools.OkMeta(map[string]any{
			"output":    out,
			"exit_code": exitCode,
			"note":      "命令未正常结束（退出码 " + itoaExit(exitCode) + "）",
		}, meta), nil
	}

	return tools.OkMeta(map[string]any{
		"output":    out,
		"exit_code": exitCode,
	}, meta), nil
}

// RegisterTerminal 将终端工具注册到注册中心。
func RegisterTerminal(reg *tools.Registry, fs *FS) {
	reg.Register(NewTerminalTool(fs))
}

// cmdFDPrefix 匹配重定向前的文件描述符前缀（2>、>>、&> 等），剥离后再判路径。
var cmdFDPrefix = regexp.MustCompile(`^([0-9]?>>?|<|&>>?)`)

// cmdPercentVar 匹配 Windows cmd 的 %VAR% 环境变量引用（无法静态解析，保守判越界）。
var cmdPercentVar = regexp.MustCompile(`%[A-Za-z_][A-Za-z0-9_]*%`)

// cmdDrivePath 匹配 Windows 盘符绝对路径（C:\x 或 C:/x）。
var cmdDrivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// commandTouchesOutside 启发式扫描命令文本，判断是否引用了工作区之外的路径。
// 覆盖：绝对路径、../ 穿越、~ 主目录、重定向目标、cd 到区外目录、
// Windows 下 MSYS 根路径（/c/...）以及无法静态解析的 $VAR/%VAR% 路径。
// 启发式宁可误报（多弹一次审批），不可漏报（静默越界）；
// shell 引用/拼接等形式的手法不在此列，最终防线是审批环节的人工确认。
func (f *FS) commandTouchesOutside(cmd string) bool {
	if strings.TrimSpace(cmd) == "" {
		return false
	}
	f.mu.Lock()
	root, allow := f.root, f.allowOutside
	f.mu.Unlock()
	if allow || strings.TrimSpace(root) == "" {
		return false
	}
	home, _ := os.UserHomeDir()

	for _, raw := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		tok := cmdFDPrefix.ReplaceAllString(raw, "")
		tok = strings.Trim(tok, "\"'`<>;,(){}[]&|")
		if tok == "" {
			continue
		}
		if f.tokenOutside(root, home, tok) {
			return true
		}
	}
	return false
}

// tokenOutside 判断单个命令 token 是否为指向工作区之外的路径引用。
func (f *FS) tokenOutside(root, home, tok string) bool {
	if len(tok) > 1 && strings.HasPrefix(tok, "-") {
		return false // 选项（-rf、--force）
	}
	pathLike := strings.ContainsAny(tok, `/\`) || strings.Contains(tok, "..") ||
		strings.HasPrefix(tok, "~") || cmdDrivePath.MatchString(tok)
	if !pathLike {
		return false
	}
	if strings.Contains(tok, "$") || cmdPercentVar.MatchString(tok) {
		return true // 变量路径无法静态解析，保守判定为越界
	}
	// 空设备只吞不吐，不构成数据越界。
	if tok == "/dev/null" || strings.EqualFold(tok, "nul") {
		return false
	}
	switch {
	case strings.HasPrefix(tok, "~"):
		if home == "" {
			return true
		}
		tok = filepath.Join(home, strings.TrimPrefix(tok, "~"))
	case runtime.GOOS == "windows" && strings.HasPrefix(tok, "/"):
		// cmd.exe 的短开关（/y、/q）；其余根路径按 MSYS/Git-Bash 处理，
		// 会映射到 Git 安装目录，必不在工作区内。
		if regexp.MustCompile(`^/[A-Za-z0-9]+$`).MatchString(tok) {
			return false
		}
		return true
	}

	var abs string
	if filepath.IsAbs(tok) {
		abs = filepath.Clean(tok)
	} else {
		abs = filepath.Join(root, tok)
	}
	return f.absOutside(abs)
}

// absOutside 判断绝对路径是否落在 root 之外。
//
// 2.7 起**不再自带一份围栏逻辑**：原实现是 checkScope 的复制品
// （同样两层、同样对 root 与 realRoot 各判一次），只少了「未选工作区 /
// 显式放行」的提前返回 —— 于是两份逻辑迟早会分叉。
//
// 现在直接委托 checkScope。差别只有一处：checkScope 在 root == "" 或
// allowOutside 时**放行**，而调用方 commandTouchesOutside 在那之前就已经
// 返回 false 了（它自己判了 allow 与空 root），所以行为等价。
func (f *FS) absOutside(abs string) bool {
	return f.checkScope(abs) != nil
}
