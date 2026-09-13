package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeforge/pkg/tools"
)

// Snapshot 是一次文件写入前的快照，用于撤销。
type Snapshot struct {
	Path    string    `json:"path"`
	Existed bool      `json:"existed"`
	Content []byte    `json:"-"`
	Time    time.Time `json:"time"`
}

// FS 是文件工具共享的工作区状态（含撤销栈）。
type FS struct {
	root         string
	allowOutside bool
	mu           sync.Mutex
	undo         []Snapshot
	maxUndo      int
}

// NewFS 构造文件工具工作区。root 为空表示「未选择工作区」，
// 所有文件工具在 选择工作区 之前拒绝执行。
func NewFS(root string) *FS {
	if root = strings.TrimSpace(root); root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	return &FS{root: root, maxUndo: 100}
}

// Root 返回工作区根目录（空字符串表示未选择工作区）。
func (f *FS) Root() string { return f.root }

// noWorkspace 未选择工作区时返回统一的错误结果，否则返回 nil。
func (f *FS) noWorkspace() *tools.ToolResult {
	f.mu.Lock()
	root := f.root
	f.mu.Unlock()
	if strings.TrimSpace(root) != "" {
		return nil
	}
	return tools.Err("未选择工作区：请先在界面点击「选择工作区」后再执行文件操作")
}

// SetRoot 热切换工作区根目录（清空撤销栈，避免跨工作区误撤销）。
// root 为空表示清除工作区（回到「未选择」状态，不解析为当前目录）。
func (f *FS) SetRoot(root string) {
	root = strings.TrimSpace(root)
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.root = root
	f.undo = nil
}

// SetAllowOutside 设置是否允许访问工作区之外的路径。
// 默认 false：文件与命令工具的目标必须落在工作区内。
func (f *FS) SetAllowOutside(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.allowOutside = v
}

// AllowOutside 返回当前是否允许越出工作区访问。
func (f *FS) AllowOutside() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.allowOutside
}

// Resolve 将入参路径解析为绝对路径（相对路径基于工作区根）。
// 注意：本方法只做路径拼接，不做越界校验 —— 工具请改用 ResolveChecked。
func (f *FS) Resolve(p string) string {
	f.mu.Lock()
	root := f.root
	f.mu.Unlock()

	if strings.TrimSpace(p) == "" {
		return root
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(root, p)
}

// ErrOutsideWorkspace 表示解析后的路径落在了工作区之外。
var ErrOutsideWorkspace = errors.New(
	"路径超出工作区范围（如需访问工作区外的文件，请在 config/local.yaml 中设置 security.allow_outside_workspace: true）")

// within 判断 path 是否位于 root 之内（含 root 自身）。
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveReal 返回 path 的真实路径：自 path 起逐级向上找到第一个能被
// EvalSymlinks 成功解析的祖先，把真实祖先与尚未存在的尾部组件拼回。
// 只对最终组件做 EvalSymlinks 会漏掉「区内软链指向区外、目标文件尚不存在」
// 的写入绕过（新建文件的 EvalSymlinks 必然失败）。
func resolveReal(path string) string {
	suffix := ""
	cur := path
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return real
			}
			return filepath.Join(real, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path // 已到根仍解析失败，退回原路径
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

// checkScope 对已解析的绝对路径做工作区约束校验（词法层 + 软链接层）。
func (f *FS) checkScope(abs string) error {
	f.mu.Lock()
	root, allow := f.root, f.allowOutside
	f.mu.Unlock()

	// 未选择工作区由 noWorkspace() 拦截；显式放行则不做约束。
	if root == "" || allow {
		return nil
	}

	realRoot := root
	if r, err := filepath.EvalSymlinks(root); err == nil {
		realRoot = r
	}

	if !within(root, abs) && !within(realRoot, abs) {
		return fmt.Errorf("%w: %s", ErrOutsideWorkspace, abs)
	}
	// 词法上在区内，再解析真实路径，挡住「区内软链指向区外」。
	real := resolveReal(abs)
	if !within(root, real) && !within(realRoot, real) {
		return fmt.Errorf("%w: %s", ErrOutsideWorkspace, abs)
	}
	return nil
}

// ResolveChecked 在 Resolve 的基础上做工作区约束校验。
// 返回的 error 非 nil 时，调用方必须直接把错误回给模型，不要执行任何文件操作。
//
// 校验分两层：
//  1. 词法层：Clean 之后的绝对路径必须落在 root 之内，挡住 `..\..\x` 与 `C:\Windows\...`；
//  2. 软链接层：解析真实路径（含尚未存在的尾部组件），挡住「工作区内的软链指向区外」。
//
// 边界会把 root 的软链接解析结果一并视为合法（如 macOS 的 /var → /private/var），
// 避免 root 自身带软链接时把区内路径误判为越界。
func (f *FS) ResolveChecked(p string) (string, error) {
	return f.ResolveCheckedCtx(context.Background(), p)
}

// ResolveCheckedCtx 是 ResolveChecked 的 context 感知版本：
// 当执行器已在人工审批通过后注入放行标记（WithScopeApproved）时，
// 对这一次调用跳过【工作区越界】拦截——但子智能体围栏仍然生效：
// 围栏守护的是子任务之间的互斥范围，与单次调用是否获批语义不同，
// 必须绝对、不可被审批穿透。
func (f *FS) ResolveCheckedCtx(ctx context.Context, p string) (string, error) {
	abs := f.Resolve(p)
	if err := f.checkSubagentScope(ctx, abs); err != nil {
		return abs, err
	}
	if tools.ScopeApprovedFrom(ctx) {
		return abs, nil
	}
	if err := f.checkScope(abs); err != nil {
		return abs, err
	}
	return abs, nil
}

// checkSubagentScope 执行子智能体路径围栏校验：当 context 中带有
// WithSubagentScope 注入的范围时，解析后的目标路径必须落在其中一个允许路径内。
// 这是防止子智能体越权访问其它子任务代码范围的最终防线（工具层强制，不依赖模型自觉）。
func (f *FS) checkSubagentScope(ctx context.Context, abs string) error {
	sc, ok := tools.SubagentScopeFrom(ctx)
	if !ok {
		return nil
	}
	f.mu.Lock()
	root := f.root
	f.mu.Unlock()
	for _, p := range sc.Allowed {
		allowed := filepath.Clean(strings.TrimSpace(p))
		if allowed == "" {
			continue
		}
		if !filepath.IsAbs(allowed) {
			allowed = filepath.Join(root, allowed)
		}
		if within(allowed, abs) {
			return nil
		}
	}
	return fmt.Errorf("子智能体越权：目标 %s 不在其允许范围 %v 内（围栏 %s）", abs, sc.Allowed, sc.Mode)
}

// outsidePath 判断路径参数是否越出工作区（供 ScopeChecker 使用）。
// allowOutside 开启或未选择工作区时不视为越界（后者由 noWorkspace 拦截）。
func (f *FS) outsidePath(p string) bool {
	f.mu.Lock()
	root, allow := f.root, f.allowOutside
	f.mu.Unlock()
	if allow || strings.TrimSpace(root) == "" {
		return false
	}
	return f.checkScope(f.Resolve(p)) != nil
}

// OutsideScopePath 从工具参数中取 path 字段并判断是否越出工作区。
func (f *FS) OutsideScopePath(args json.RawMessage) bool {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return true // 参数不可解析时保守判定为需要审批
	}
	return f.outsidePath(p.Path)
}

// snapshot 在写入前记录文件快照。
func (f *FS) snapshot(path string) {
	data, err := os.ReadFile(path)
	snap := Snapshot{Path: path, Time: time.Now()}
	if err == nil {
		snap.Existed = true
		snap.Content = data
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undo = append(f.undo, snap)
	if len(f.undo) > f.maxUndo {
		f.undo = f.undo[len(f.undo)-f.maxUndo:]
	}
}

// Undo 撤销最近一次文件写入，返回被还原的路径。
func (f *FS) Undo() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.undo) == 0 {
		return "", false
	}
	snap := f.undo[len(f.undo)-1]
	f.undo = f.undo[:len(f.undo)-1]

	if snap.Existed {
		_ = os.MkdirAll(filepath.Dir(snap.Path), 0o755)
		if err := os.WriteFile(snap.Path, snap.Content, 0o644); err != nil {
			return snap.Path, false
		}
	} else {
		_ = os.Remove(snap.Path)
	}
	return snap.Path, true
}

// UndoDepth 返回当前可撤销步数。
func (f *FS) UndoDepth() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.undo)
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

// ReadFileTool 读取文件内容。
type ReadFileTool struct{ fs *FS }

// NewReadFileTool 构造 read_file 工具。
func NewReadFileTool(fs *FS) *ReadFileTool { return &ReadFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *ReadFileTool) Name() string { return "read_file" }

// Description 实现 tools.Tool。
func (t *ReadFileTool) Description() string {
	return "读取指定文本文件的内容，可选 start_line / end_line 限定行范围（1-based，含端点）。"
}

// InputSchema 实现 tools.Tool。
func (t *ReadFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "文件路径（相对工作区或绝对路径，必须位于工作区内）", true).
		Int("start_line", "起始行号，1-based，可选", false).
		Int("end_line", "结束行号，1-based，含端点，可选", false).
		Build()
}

// IsReadOnly 声明只读。
func (t *ReadFileTool) IsReadOnly() bool { return true }

// OutsideScope 实现 tools.ScopeChecker。
func (t *ReadFileTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

// Execute 实现 tools.Tool。
func (t *ReadFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%v", err), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tools.Err("读取文件失败: %v", err), nil
	}
	content := string(data)

	if p.StartLine > 0 || p.EndLine > 0 {
		lines := strings.Split(content, "\n")
		start := p.StartLine
		if start <= 0 {
			start = 1
		}
		end := p.EndLine
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) {
			return tools.Ok(""), nil
		}
		content = strings.Join(lines[start-1:end], "\n")
	}

	return tools.OkMeta(content, map[string]any{"path": path, "bytes": len(data)}), nil
}

// ---------------------------------------------------------------------------
// list_dir
// ---------------------------------------------------------------------------

// ListDirTool 列出目录内容。
type ListDirTool struct{ fs *FS }

// NewListDirTool 构造 list_dir 工具。
func NewListDirTool(fs *FS) *ListDirTool { return &ListDirTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *ListDirTool) Name() string { return "list_dir" }

// Description 实现 tools.Tool。
func (t *ListDirTool) Description() string {
	return "列出目录下的文件与子目录（名称 / 类型 / 大小）。"
}

// InputSchema 实现 tools.Tool。
func (t *ListDirTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "目录路径（相对工作区或绝对路径，必须位于工作区内），留空表示工作区根目录", false).
		Bool("recursive", "是否递归列出（默认 false）", false).
		Build()
}

// IsReadOnly 声明只读。
func (t *ListDirTool) IsReadOnly() bool { return true }

// OutsideScope 实现 tools.ScopeChecker。
func (t *ListDirTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

type dirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Execute 实现 tools.Tool。
func (t *ListDirTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	dir, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%v", err), nil
	}

	entries := make([]dirEntry, 0, 64)
	add := func(path string, info os.FileInfo) {
		entries = append(entries, dirEntry{
			Name:  info.Name(),
			Path:  path,
			IsDir: info.IsDir(),
			Size:  info.Size(),
		})
	}

	if p.Recursive {
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if path == dir {
				return nil
			}
			if info.IsDir() && skipDir(info.Name()) {
				return filepath.SkipDir
			}
			add(path, info)
			return nil
		})
	} else {
		items, err := os.ReadDir(dir)
		if err != nil {
			return tools.Err("读取目录失败: %v", err), nil
		}
		for _, it := range items {
			info, err := it.Info()
			if err != nil {
				continue
			}
			add(filepath.Join(dir, it.Name()), info)
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return tools.OkMeta(entries, map[string]any{"dir": dir, "count": len(entries)}), nil
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

// WriteFileTool 写入（覆盖）文件。
type WriteFileTool struct{ fs *FS }

// NewWriteFileTool 构造 write_file 工具。
func NewWriteFileTool(fs *FS) *WriteFileTool { return &WriteFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *WriteFileTool) Name() string { return "write_file" }

// Description 实现 tools.Tool。
func (t *WriteFileTool) Description() string {
	return "将内容写入指定文件（覆盖式）。若文件不存在则创建，父目录自动补齐。写入前会记录快照以支持撤销。"
}

// InputSchema 实现 tools.Tool。
func (t *WriteFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "文件路径", true).
		Str("content", "要写入的完整内容", true).
		Build()
}

// PreviewDiff 返回本次写入的 Unified Diff。
func (t *WriteFileTool) PreviewDiff(args json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path, err := t.fs.ResolveChecked(p.Path)
	if err != nil {
		return "", err
	}
	old, _ := os.ReadFile(path)
	return UnifiedDiff(path, path, string(old), p.Content), nil
}

// OutsideScope 实现 tools.ScopeChecker。
func (t *WriteFileTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

// Execute 实现 tools.Tool。
func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if p.Path == "" {
		return tools.Err("path 不能为空"), nil
	}
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%v", err), nil
	}
	t.fs.snapshot(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return tools.Err("创建目录失败: %v", err), nil
	}
	if err := os.WriteFile(path, []byte(p.Content), 0o644); err != nil {
		return tools.Err("写入文件失败: %v", err), nil
	}
	return tools.OkMeta(map[string]any{
		"path":  path,
		"bytes": len(p.Content),
	}, map[string]any{"path": path}), nil
}

// ---------------------------------------------------------------------------
// edit_file
// ---------------------------------------------------------------------------

// EditFileTool 以「读→旧串替换→写」方式编辑文件。
type EditFileTool struct{ fs *FS }

// NewEditFileTool 构造 edit_file 工具。
func NewEditFileTool(fs *FS) *EditFileTool { return &EditFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *EditFileTool) Name() string { return "edit_file" }

// Description 实现 tools.Tool。
func (t *EditFileTool) Description() string {
	return "对文件做精确字符串替换：将 old_string 替换为 new_string。old_string 必须在文件中唯一出现（除非 replace_all=true）。"
}

// InputSchema 实现 tools.Tool。
func (t *EditFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "文件路径", true).
		Str("old_string", "要被替换的原文（需与文件内容完全一致，含缩进）", true).
		Str("new_string", "替换后的新文本", true).
		Bool("replace_all", "是否替换全部匹配（默认 false，要求唯一匹配）", false).
		Build()
}

type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// PreviewDiff 返回本次编辑的 Unified Diff。
func (t *EditFileTool) PreviewDiff(args json.RawMessage) (string, error) {
	var p editArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path, err := t.fs.ResolveChecked(p.Path)
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	updated, _, err := applyEdit(string(old), p)
	if err != nil {
		return "", err
	}
	return UnifiedDiff(path, path, string(old), updated), nil
}

// OutsideScope 实现 tools.ScopeChecker。
func (t *EditFileTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

// Execute 实现 tools.Tool。
func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p editArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%v", err), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tools.Err("读取文件失败: %v", err), nil
	}
	updated, count, err := applyEdit(string(data), p)
	if err != nil {
		return tools.Err("%v", err), nil
	}
	t.fs.snapshot(path)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return tools.Err("写入文件失败: %v", err), nil
	}
	return tools.OkMeta(map[string]any{
		"path":         path,
		"replacements": count,
	}, map[string]any{"path": path}), nil
}

// applyEdit 执行替换并返回新内容与替换次数。
func applyEdit(content string, p editArgs) (string, int, error) {
	if p.OldString == "" {
		return "", 0, fmt.Errorf("old_string 不能为空")
	}
	count := strings.Count(content, p.OldString)
	if count == 0 {
		return "", 0, fmt.Errorf("未在文件中找到 old_string")
	}
	if count > 1 && !p.ReplaceAll {
		return "", 0, fmt.Errorf("old_string 在文件中出现 %d 次，不唯一；请扩大上下文或设置 replace_all=true", count)
	}
	if p.ReplaceAll {
		return strings.ReplaceAll(content, p.OldString, p.NewString), count, nil
	}
	return strings.Replace(content, p.OldString, p.NewString, 1), 1, nil
}

// ---------------------------------------------------------------------------
// delete_file
// ---------------------------------------------------------------------------

// DeleteFileTool 删除文件。
type DeleteFileTool struct{ fs *FS }

// NewDeleteFileTool 构造 delete_file 工具。
func NewDeleteFileTool(fs *FS) *DeleteFileTool { return &DeleteFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *DeleteFileTool) Name() string { return "delete_file" }

// Description 实现 tools.Tool。
func (t *DeleteFileTool) Description() string {
	return "删除指定文件（不递归删除目录）。删除前会记录快照以支持撤销。"
}

// InputSchema 实现 tools.Tool。
func (t *DeleteFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "要删除的文件路径", true).
		Build()
}

// OutsideScope 实现 tools.ScopeChecker。
func (t *DeleteFileTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

// Execute 实现 tools.Tool。
func (t *DeleteFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%v", err), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return tools.Err("文件不存在: %v", err), nil
	}
	if info.IsDir() {
		return tools.Err("delete_file 不支持删除目录: %s", path), nil
	}
	t.fs.snapshot(path)
	if err := os.Remove(path); err != nil {
		return tools.Err("删除失败: %v", err), nil
	}
	return tools.OkMeta(map[string]any{"path": path, "deleted": true}, map[string]any{"path": path}), nil
}

// RegisterFS 将全部文件工具注册到注册中心。
func RegisterFS(reg *tools.Registry, fs *FS) {
	reg.Register(NewReadFileTool(fs))
	reg.Register(NewListDirTool(fs))
	reg.Register(NewWriteFileTool(fs))
	reg.Register(NewEditFileTool(fs))
	reg.Register(NewDeleteFileTool(fs))
}

// skipDir 判断是否跳过某些目录（供检索与遍历共用）。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "vendor", ".idea", ".vscode", "__pycache__", ".codeforge":
		return true
	}
	return false
}
