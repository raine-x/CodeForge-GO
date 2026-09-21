package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"codeforge/pkg/errs"
	"codeforge/pkg/platform"
	"codeforge/pkg/tools"
)

// TerminalTool 跨平台异步执行 Shell 命令并收集输出。
type TerminalTool struct{ fs *FS }

// NewTerminalTool 构造 run_command 工具。
func NewTerminalTool(fs *FS) *TerminalTool { return &TerminalTool{fs: fs} }

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
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellArgs := platform.Shell()
	argv := append(append([]string{}, shellArgs...), p.Command)

	dir, err := t.fs.ResolveCheckedCtx(ctx, p.Cwd)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("执行命令", err)), nil
	}
	cmd := exec.CommandContext(cctx, shell, argv...)
	cmd.Dir = dir

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	exitCode := 0
	switch {
	case runErr == nil:
		exitCode = 0
	case cctx.Err() == context.DeadlineExceeded:
		exitCode = -2
	default:
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}

	out := buf.String()
	meta := map[string]any{
		"exit_code":   exitCode,
		"duration_ms": elapsed.Milliseconds(),
		"shell":       shell,
	}

	if cctx.Err() == context.DeadlineExceeded {
		return tools.OkMeta(map[string]any{
			"output":    out,
			"exit_code": exitCode,
			"note":      "命令执行超时，已被终止",
		}, meta), nil
	}
	if runErr != nil && exitCode < 0 {
		return tools.OkMeta(map[string]any{
			"output":    out,
			"exit_code": exitCode,
			"note":      runErr.Error(),
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
	return f.absOutside(root, abs)
}

// absOutside 判断绝对路径（词法层 + 真实路径层）是否落在 root 之外。
func (f *FS) absOutside(root, abs string) bool {
	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}
	if !within(root, abs) && !within(realRoot, abs) {
		return true
	}
	real := resolveReal(abs)
	return !within(root, real) && !within(realRoot, real)
}
