package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codeforge/pkg/errs"
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
	// readSeen 是「哪个会话读过/改过哪个文件」的登记表，用于先读后写约束。
	// 整个工作区共用一份 FS，故键必须带上会话 ID：子智能体与各会话之间
	// 不能拿别人读过的原文下自己的笔。
	readSeen map[string]seenEntry
}

// NewFS 构造文件工具工作区。root 为空表示「未选择工作区」，
// 所有文件工具在 选择工作区 之前拒绝执行。
func NewFS(root string) *FS {
	if root = strings.TrimSpace(root); root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	return &FS{root: root, maxUndo: 100, readSeen: map[string]seenEntry{}}
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
	f.readSeen = map[string]seenEntry{}
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

// snapshot 在写入前记录文件快照，并（可选）上报给会话级检查点槽。
//
// 两条路径的分工：
//   - 内存 undo 栈：进程内「撤销上一步」，会话结束即失效；
//   - 检查点槽（ctx）：按 (会话, 步骤, 路径) 落库，支持跨重启、按步回滚，
//     是「回退回某条用户消息之前」的基础（Plan.md #4）。
//
// 上报必须发生在写入之前：sink 拿到的是读出的旧内容，晚一步就只能读到新内容。
func (f *FS) snapshot(ctx context.Context, path string) {
	data, err := os.ReadFile(path)
	snap := Snapshot{Path: path, Time: time.Now()}
	if err == nil {
		snap.Existed = true
		snap.Content = data
	}
	f.mu.Lock()
	f.undo = append(f.undo, snap)
	if len(f.undo) > f.maxUndo {
		f.undo = f.undo[len(f.undo)-f.maxUndo:]
	}
	f.mu.Unlock()

	if ctx == nil {
		return
	}
	if sink, ok := tools.CheckpointSinkFrom(ctx); ok {
		sink(tools.CheckpointEvent{
			Path:       path,
			Existed:    snap.Existed,
			OldContent: string(data), // 文件不存在时 data 为 nil，转成空串
		})
	}
}

// Undo 撤销最近一次文件写入，返回被还原的路径。
//
// 撤销会把文件内容**改回**旧值，因此所有相关会话的指纹都必须一起刷新 ——
// 否则「读 → 写 → 撤销 → 再写」会在最后一步被判成「被外部修改」而失败。
// 快照里就有还原后的内容（snap.Content），直接拿来算，不必重读磁盘。
func (f *FS) Undo() (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.undo) == 0 {
		return "", false
	}
	snap := f.undo[len(f.undo)-1]
	f.undo = f.undo[:len(f.undo)-1]

	// 所有见过这个路径的会话，其指纹都指向「改动前」的内容。
	// 撤销把它们统一对齐到还原后的内容。
	// seenKey 的结构是 sessionID + "\n" + path，所以匹配后缀时要带前导 \n，
	// 否则路径 "a.go" 会误匹配到 "xa.go"。
	suffix := "\n" + snap.Path
	restored := snap.Existed
	var sum string
	var size int64
	if snap.Existed {
		sum, size = fingerprint(snap.Content)
	}
	for k := range f.readSeen {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		if restored {
			f.readSeen[k] = seenEntry{sum: sum, size: size}
		} else {
			// 文件被撤销成「原本不存在」，指纹无从谈起 —— 删掉登记，
			// 后续写入会走「新建文件」路径（requireReadSeen 对不存在的文件放行）。
			delete(f.readSeen, k)
		}
	}

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
// 先读后写登记
// ---------------------------------------------------------------------------

// maxReadSeen 是登记表条目上限，只为封住无界增长，不做精细淘汰：
// 淘汰的后果仅仅是「模型需要重读一次文件」，不会丢任何数据。
const maxReadSeen = 20000

func seenKey(sessionID, path string) string { return sessionID + "\n" + path }

// seenEntry 记录「本会话读过这个文件」以及**读到的内容指纹**。
//
// 为什么不只存 bool：只存 bool 时，「读 A → 外部改 A → Agent 基于旧内容写 A」
// 会被静默放行 —— Agent 覆盖掉外部的改动而没有任何报错，数据丢失且无人察觉。
// 存指纹后，写前比对即可发现。
type seenEntry struct {
	sum  string // 内容 SHA-256（取前 16 字节十六进制，足够判别且省内存）
	size int64  // 快速预筛：大小变了就不必算哈希
}

func (e seenEntry) matches(sum string, size int64) bool {
	return e.size == size && e.sum == sum
}

// sizeDelta 只在大小确实变了时才给出字节数。
// 常见情况是「改了几个字符、大小没变」，这时报「2 字节 → 2 字节」纯属噪音，
// 还显得自相矛盾。
func sizeDelta(before, after int64) string {
	if before == after {
		return ""
	}
	return fmt.Sprintf("（读取时 %d 字节，现为 %d 字节）", before, after)
}

// fingerprint 算内容指纹，size 一并返回供 seenEntry 预筛。
func fingerprint(data []byte) (string, int64) {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16]), int64(len(data))
}

// markReadSeen 登记「本会话见过该文件的当前内容」：读成功算，
// 自己刚改完也算（下一步往往是在刚才的改动上继续）。
//
// content 必须是**调用方手上那份内容的字节**，不重新读盘：
//   - read_file 传刚 os.ReadFile 出来的 data
//   - write_file / edit_file 传**刚写进去的新内容**
//
// 后者是关键：这样「写后刷新指纹」自然成立，不需要额外一行刷新逻辑。
// 若不刷新，Agent 改一次文件后指纹就与磁盘实际不符，第二次写会被判成
// 「被外部修改」而**永久锁死这个 Agent**，且报错极具误导性
// （用户会以为是别的程序干的）。
func (f *FS) markReadSeen(ctx context.Context, path string, content []byte) {
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return
	}
	sum, size := fingerprint(content)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.readSeen) >= maxReadSeen {
		f.readSeen = map[string]seenEntry{}
	}
	f.readSeen[seenKey(sc.SessionID, path)] = seenEntry{sum: sum, size: size}
}

// requireReadSeen 是写操作的先读后写闸门，两道校验：
//
//  1. 本会话读过吗 —— 没读过就改，依据的是记忆里（多半是压缩后摘要里）
//     的旧版本，整份文件会按记忆重排一遍，界面上就是 +2200/-2170。
//  2. 读完之后内容变过吗 —— 变过就说明磁盘现状已不是模型看到的那份，
//     此时的精确替换会覆盖掉这期间别人的改动。
//
// 两种情形不拦：
//   - 目标文件不存在（新建）：没有可丢的旧内容；
//   - 调用不在会话运行域内：无法判定「谁读过」，此时拦截只会让工具不可用。
func (f *FS) requireReadSeen(ctx context.Context, path string, existed bool) error {
	if !existed {
		return nil
	}
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return nil
	}
	f.mu.Lock()
	entry, seen := f.readSeen[seenKey(sc.SessionID, path)]
	f.mu.Unlock()

	if !seen {
		return fmt.Errorf("本会话还没读过 %s，不能凭记忆改写。先用 read_file 读取（大文件按返回末尾的行号窗口分段读），"+
			"看到原文后再提交精确替换。", path)
	}

	// 第 2 道：指纹比对。读盘失败按「已变」处理（fail-closed）——
	// 拿不到现状就没法证明现状没变，不能因此放行。
	cur, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("无法确认 %s 读取之后是否被修改：%w", path, err)
	}
	sum, size := fingerprint(cur)
	if !entry.matches(sum, size) {
		return fmt.Errorf("%s 在本会话读取之后被外部修改过%s。"+
			"请先重新 read_file 看到最新内容，再基于它提交修改 —— "+
			"否则会覆盖掉这段时间别人做的改动。", path, sizeDelta(entry.size, size))
	}
	return nil
}

// ForgetReads 作废该会话的全部阅读登记（实现 tools.ReadGate，由压缩路径调用）。
//
// 原文一旦被挤出送模视图，「这个会话读过它」就不再是事实了：此时放任模型
// 覆盖写入，它依据的是摘要里的印象 —— 正是整份重排的来源。
func (f *FS) ForgetReads(sessionID string) {
	prefix := sessionID + "\n"
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.readSeen {
		if strings.HasPrefix(k, prefix) {
			delete(f.readSeen, k)
		}
	}
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

// 读取窗口的限幅参数。
const (
	// defaultReadLines 是单次读取的最大行数。
	defaultReadLines = 2000
	// maxReadBytes 是 read_file 自身返回值的字节上限。
	//
	// 它必须明显低于执行器的输出限幅（默认 32KiB）：撞到这里由本工具收尾，
	// 并明确告诉模型「共多少行、读到第几行、怎么接着读」；
	// 若放任长度长到限幅以上，执行器会按**字节位置**硬切一刀，模型拿到的
	// 是一份悄悄少了一截的文件原文 —— 凭这份残本再整体覆盖写回，
	// 尾部内容就静默丢了。
	maxReadBytes = 24 * 1024
	// maxLineRunes 是单行长度上限：压缩过的 JS/CSS 一行可达数百 KB，
	// 单行就足以吃满整个上下文窗口。
	maxLineRunes = 2000
)

// numberLines 把文本渲染成带行号的视图（cat -n 风格），并按 [start, end] 截取。
//
// 行号是精确替换的锚点：模型只有在「第 1234 行写着什么」这个坐标系里，
// 才能在几百行的文件里准确定位到要改的那一处；没有锚点时它只能整体重写。
// 返回实际读到的末行号；末行号小于 total 即说明还有内容未读。
func numberLines(content string, start, end int) (text string, last, total int, lineTooLong int) {
	lines := splitLines(content) // 已兼容 CRLF、丢弃末尾空行
	total = len(lines)
	if start < 1 {
		start = 1
	}
	if start > total {
		return "", 0, total, 0
	}
	if end > total {
		end = total
	}

	var sb strings.Builder
	written := 0
	for i := start - 1; i < end; i++ {
		line := lines[i]
		if n := utf8.RuneCountInString(line); n > maxLineRunes {
			line = string([]rune(line)[:maxLineRunes]) + fmt.Sprintf("…（本行共 %d 字，已截断）", n)
			if lineTooLong == 0 {
				lineTooLong = i + 1
			}
		}
		rendered := fmt.Sprintf("%6d\t%s\n", i+1, line)
		if written+len(rendered) > maxReadBytes {
			// 字节预算用尽：就此收尾，交给调用方提示续读位置。
			break
		}
		written += len(rendered)
		sb.WriteString(rendered)
		last = i + 1
	}
	return sb.String(), last, total, lineTooLong
}

// readWindow 把入参行范围归一为「本工具能够返回」的窗口。
// 只给 start_line 时向后取满一个窗口；什么都不给时取首个窗口。
func readWindow(startLine, endLine int) (start, end int) {
	start = startLine
	if start < 1 {
		start = 1
	}
	end = endLine
	if end <= 0 || end > start+defaultReadLines-1 {
		end = start + defaultReadLines - 1
	}
	return start, end
}

// ReadFileTool 读取文件内容。
type ReadFileTool struct{ fs *FS }

// NewReadFileTool 构造 read_file 工具。
func NewReadFileTool(fs *FS) *ReadFileTool { return &ReadFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *ReadFileTool) Name() string { return "read_file" }

// Description 实现 tools.Tool。
func (t *ReadFileTool) Description() string {
	return "按行读取文件，每行以「行号 + 制表符」前缀标注（cat -n 风格）。" +
		"单次最多返回 2000 行，未读满时用 start_line 继续；" +
		"对 .docx / .pptx / .xlsx / .pdf 等文档会自动提取其中的文字（PDF 仅文本型，扫描件无法提取）。" +
		"编辑文件时请从这些行号定位目标片段，old_string 仍需逐字照抄（不含行号前缀）。"
}

// InputSchema 实现 tools.Tool。
func (t *ReadFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "文件路径（相对工作区或绝对路径，必须位于工作区内）", true).
		Int("start_line", "起始行号，1-based，可选（默认从第 1 行）", false).
		Int("end_line", "结束行号，1-based，含端点，可选（单次上限 2000 行）", false).
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
		return tools.Err("%s", errs.FriendlyOr("读取文件", err)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tools.Err("读取文件失败: %v", err), nil
	}

	content := string(data)
	extracted := false
	// 多格式文档（docx/pptx/xlsx/pdf）：自动提取文字后再按行窗口截取。
	// 提取失败（扫描件 PDF、损坏的包等）把错误如实回给模型，不降级为乱码。
	if text, ok, exErr := extractDocText(data, p.Path); ok {
		if exErr != nil {
			return tools.Err("文档文字提取失败: %v", exErr), nil
		}
		content, extracted = text, true
	}
	// 只有「看到的就是文件本身」才算读过：文档提取出的是残缺正文，
	// 拿它当依据写回原文件同样是在赌。
	if !extracted {
		t.fs.markReadSeen(ctx, path, data)
	}

	start, end := readWindow(p.StartLine, p.EndLine)
	text, last, total, longLine := numberLines(content, start, end)

	meta := map[string]any{"path": path, "bytes": len(data), "total_lines": total}
	if extracted {
		meta["extracted"] = true
	}
	switch {
	case total == 0:
		return tools.OkMeta("（文件为空）", meta), nil
	case last == 0:
		// 起始行越界：明确告知文件真实长度，避免模型反复往后探。
		return tools.OkMeta(fmt.Sprintf(
			"（第 %d 行已超出文件末尾，该文件共 %d 行）", start, total), meta), nil
	}
	if longLine > 0 {
		text += fmt.Sprintf("（注意：第 %d 行超长已截断，该行无法用于精确替换的原文比对。）\n", longLine)
	}
	if last < total {
		text += fmt.Sprintf("（该文件共 %d 行，本次返回第 %d-%d 行；剩余 %d 行未读，需要时用 start_line=%d 继续。）",
			total, start, last, total-last, last+1)
		meta["truncated"] = true
	}
	meta["from"], meta["to"] = start, last
	return tools.OkMeta(text, meta), nil
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
		return tools.Err("%s", errs.FriendlyOr("列出目录", err)), nil
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
	return "整体覆盖写入文件：只在创建新文件或确需重写整份内容时使用（覆盖已有文件时，本会话内必须先读过它）。" +
		"修改已有文件的局部内容一律用「编辑文件」的精确替换（多处改动就一次提交多处替换），" +
		"覆盖式写入会把整份文件重新计算一遍差异（返回里会给出 +N/-M）。"
}

// InputSchema 实现 tools.Tool。
func (t *WriteFileTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("path", "文件路径", true).
		Str("content", "要写入的完整内容；清空文件请显式传空串，省略该字段会被拒绝", true).
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

// ForgetReads 实现 tools.ReadGate（与 edit_file 共用同一份 FS 登记表）。
func (t *WriteFileTool) ForgetReads(sessionID string) { t.fs.ForgetReads(sessionID) }

// Execute 实现 tools.Tool。
func (t *WriteFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if p.Path == "" {
		return tools.Err("path 不能为空"), nil
	}
	// 区分「显式清空」与「漏传 content」：后者会把已有文件静默抹成空文件。
	if p.Content == nil {
		return tools.Err("缺少 content 参数：不写入任何内容。确需清空文件请显式传 content=\"\""), nil
	}
	content := *p.Content
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("写入文件", err)), nil
	}
	// 先取旧内容：既是为了判断目标是否已存在（先读后写闸门），
	// 也是为了把「这次到底改了多少行」回给模型。
	// 只报字节数的话，模型永远不知道自己把一份 2200 行的文件整体重写了，
	// 也就没有回到精确替换的机会 —— 界面上的 +2200/-2170 只有人看得到。
	old, readErr := os.ReadFile(path)
	existed := readErr == nil
	if err := t.fs.requireReadSeen(ctx, path, existed); err != nil {
		return tools.Err("%s", errs.FriendlyOr("写入文件", err)), nil
	}
	t.fs.snapshot(ctx, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return tools.Err("创建目录失败: %v", err), nil
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return tools.Err("写入文件失败: %v", err), nil
	}
	t.fs.markReadSeen(ctx, path, []byte(content))
	out := map[string]any{"path": path, "bytes": len(content), "created": !existed}
	if existed {
		added, removed := LineChurn(string(old), content)
		out["added"], out["removed"] = added, removed
		out["lines_before"], out["lines_after"] = len(splitLines(string(old))), len(splitLines(content))
		if hint := overwriteHint(string(old), content, added, removed); hint != "" {
			out["hint"] = hint
		}
	}
	return tools.OkMeta(out, map[string]any{"path": path}), nil
}

// overwriteChurnPercent 是「整文件覆盖」提醒的改动比例阈值（%）。
// 低于它说明这次覆盖写的确是局部改动，不值得啰嗦；高于它说明
// 这次提交把大半个文件重排了一遍 —— 通常本可以用几处精确替换完成。
const overwriteChurnPercent = 60

// overwriteHint 在覆盖式写入抖动过大时给模型一句可执行的提醒。
// 只提醒、不拦截：整份重写（新建、重构、格式转换）是合法操作。
func overwriteHint(old, new string, added, removed int) string {
	oldLines, newLines := len(splitLines(old)), len(splitLines(new))
	if oldLines == 0 {
		return ""
	}
	if added+removed == 0 {
		return "写入内容与原文件完全一致，本次没有产生任何改动：确认是否重复写了同一份内容。"
	}
	// 抖动比例：全量重写且每行都不同时为 100%（added=newLines, removed=oldLines）。
	denom := oldLines + newLines
	pct := (added + removed) * 100 / denom
	if pct < overwriteChurnPercent {
		return ""
	}
	return fmt.Sprintf(
		"本次是整文件覆盖：原 %d 行 → 新 %d 行（+ %d / - %d），抖动比例 %d%%。"+
			"若本意只是局部修改，请改用精确替换（同一文件的多处改动一次提交），"+
			"避免整份重写带入无关改动、或丢掉本次没读到的内容。",
		oldLines, newLines, added, removed, pct)
}

// ---------------------------------------------------------------------------
// edit_file
// ---------------------------------------------------------------------------

// editPair 是一处精确替换。
type editPair struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type editArgs struct {
	Path string `json:"path"`
	editPair
	// Edits 一次提交多处替换，按顺序依次生效（后一处在前一处的结果上匹配）。
	// 提供本字段时忽略顶层的 old_string / new_string。
	//
	// 为什么需要它：一次跨多处的改动若只能一处一次调用来回，
	// 「精确替换」的代价就远高于「覆盖写入整份文件」，模型会理性地选择后者，
	// 于是出现 +2200/-2170 这种把整份文件重写的提交。补齐多替换能力，
	// 才是让局部编辑在成本上真正划得来。
	Edits []editPair `json:"edits,omitempty"`
}

// EditFileTool 以「读→替换→写」方式编辑文件。
type EditFileTool struct{ fs *FS }

// NewEditFileTool 构造 edit_file 工具。
func NewEditFileTool(fs *FS) *EditFileTool { return &EditFileTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *EditFileTool) Name() string { return "edit_file" }

// Description 实现 tools.Tool。
func (t *EditFileTool) Description() string {
	return "对文件做精确字符串替换，改已有内容一律用它（本会话内未读过的文件需先读取）。" +
		"单处替换传 old_string/new_string；同一文件的多处改动放进 edits 数组一次提交（原子：任一处不匹配则整笔不写）。" +
		"old_string 需与文件原文逐字一致（含缩进与行尾），默认要求唯一匹配，重复时可设 replace_all=true。"
}

// InputSchema 实现 tools.Tool。
func (t *EditFileTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "文件路径"},
    "old_string": {"type": "string", "description": "要被替换的原文（需与文件内容完全一致，含缩进）"},
    "new_string": {"type": "string", "description": "替换后的新文本"},
    "replace_all": {"type": "boolean", "description": "是否替换全部匹配（默认 false，要求唯一匹配）"},
    "edits": {
      "type": "array",
      "description": "一次提交多处替换，按顺序依次生效；与顶层 old_string 互斥。整笔原子写入：任何一处不匹配则文件不改动。",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {"type": "string", "description": "要被替换的原文（逐字一致）"},
          "new_string": {"type": "string", "description": "替换后的新文本"},
          "replace_all": {"type": "boolean", "description": "是否替换该处的全部匹配"}
        },
        "required": ["old_string", "new_string"]
      }
    }
  },
  "required": ["path"]
}`)
}

// normalizeEdits 归一化两种入参形态为统一的替换列表。
func (p editArgs) normalizeEdits() ([]editPair, error) {
	if len(p.Edits) > 0 {
		if p.OldString != "" || p.NewString != "" {
			return nil, fmt.Errorf("edits 与顶层 old_string/new_string 不能混用，二选一")
		}
		for i, pr := range p.Edits {
			if pr.OldString == "" {
				return nil, fmt.Errorf("第 %d 处替换的 old_string 为空", i+1)
			}
		}
		return p.Edits, nil
	}
	return []editPair{p.editPair}, nil
}

// PreviewDiff 返回本次编辑的 Unified Diff。
func (t *EditFileTool) PreviewDiff(args json.RawMessage) (string, error) {
	var p editArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	pairs, err := p.normalizeEdits()
	if err != nil {
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
	updated, _, err := applyPairs(string(old), pairs)
	if err != nil {
		return "", err
	}
	return UnifiedDiff(path, path, string(old), updated), nil
}

// OutsideScope 实现 tools.ScopeChecker。
func (t *EditFileTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

// ForgetReads 实现 tools.ReadGate（与 write_file 共用同一份 FS 登记表）。
func (t *EditFileTool) ForgetReads(sessionID string) { t.fs.ForgetReads(sessionID) }

// Execute 实现 tools.Tool。
func (t *EditFileTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p editArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	pairs, err := p.normalizeEdits()
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("编辑文件", err)), nil
	}
	path, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("编辑文件", err)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return tools.Err("读取文件失败: %v", err), nil
	}
	if err := t.fs.requireReadSeen(ctx, path, true); err != nil {
		return tools.Err("%s", errs.FriendlyOr("读取文件", err)), nil
	}
	updated, count, err := applyPairs(string(data), pairs)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("编辑文件", err)), nil
	}
	t.fs.snapshot(ctx, path)
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return tools.Err("写入文件失败: %v", err), nil
	}
	t.fs.markReadSeen(ctx, path, []byte(updated))
	added, removed := LineChurn(string(data), updated)
	return tools.OkMeta(map[string]any{
		"path":         path,
		"hunks":        len(pairs),
		"replacements": count,
		"added":        added,
		"removed":      removed,
		"total_lines":  len(splitLines(updated)),
	}, map[string]any{"path": path}), nil
}

// applyPairs 依次应用多处替换；任何一处失败都不产出结果（调用方不落盘）。
func applyPairs(content string, pairs []editPair) (string, int, error) {
	out := content
	total := 0
	for i, pr := range pairs {
		next, n, err := applyOne(out, pr)
		if err != nil {
			if len(pairs) == 1 {
				return "", 0, err
			}
			return "", 0, fmt.Errorf("第 %d/%d 处替换失败：%v（本次未写入任何改动）", i+1, len(pairs), err)
		}
		out, total = next, total+n
	}
	return out, total, nil
}

// applyOne 执行单处替换，带行尾符兼容。
func applyOne(content string, pr editPair) (string, int, error) {
	if pr.OldString == "" {
		return "", 0, fmt.Errorf("old_string 不能为空")
	}
	if pr.OldString == pr.NewString {
		return "", 0, fmt.Errorf("old_string 与 new_string 完全相同，这处替换没有改动")
	}
	for _, v := range lineEndingVariants(pr) {
		count := strings.Count(content, v.OldString)
		if count == 0 {
			continue
		}
		if count > 1 && !v.ReplaceAll {
			return "", 0, fmt.Errorf("old_string 在文件中出现 %d 次，不唯一；请扩大上下文或设置 replace_all=true", count)
		}
		if v.ReplaceAll {
			return strings.ReplaceAll(content, v.OldString, v.NewString), count, nil
		}
		return strings.Replace(content, v.OldString, v.NewString, 1), 1, nil
	}
	return "", 0, fmt.Errorf("未在文件中找到 old_string。%s", nearMissHint(content, pr.OldString))
}

// lineEndingVariants 生成行尾符兼容的匹配候选。
//
// Windows 工作区里的文件常是 CRLF，而模型照抄带行号的读取结果时给出的多行
// 片段是 \n —— 逐字比对必然失配。失配几次之后模型就会退化成整文件覆盖，
// 这正是「本该局部编辑却全量重写」的起点，所以在这里兼容掉。
func lineEndingVariants(pr editPair) []editPair {
	base := editPair{OldString: pr.OldString, NewString: pr.NewString, ReplaceAll: pr.ReplaceAll}
	if !strings.Contains(pr.OldString, "\n") {
		return []editPair{base}
	}
	toCRLF := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n") }
	toLF := func(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }
	return []editPair{
		base,
		{OldString: toCRLF(pr.OldString), NewString: toCRLF(pr.NewString), ReplaceAll: pr.ReplaceAll},
		{OldString: toLF(pr.OldString), NewString: toLF(pr.NewString), ReplaceAll: pr.ReplaceAll},
	}
}

// nearMissHint 为「找不到 old_string」补一句可执行的排查方向：
// 只报首行落在第几行，不回吐文件内容（那是模型下一步 read_file 的事）。
func nearMissHint(content, oldString string) string {
	head := strings.TrimSpace(strings.SplitN(strings.ReplaceAll(oldString, "\r\n", "\n"), "\n", 2)[0])
	if head == "" {
		return "old_string 只有换行，请先读取文件确认原文。"
	}
	if utf8.RuneCountInString(head) > 60 {
		head = string([]rune(head)[:60]) + "…"
	}
	lines := splitLines(content)
	for i, l := range lines {
		if strings.TrimSpace(l) == head {
			return fmt.Sprintf("首行内容在文件第 %d 行出现过，多半是缩进或前后文不一致；用 start_line=%d 重新读取该处原文后再替换。",
				i+1, maxInt(1, i-5))
		}
	}
	return fmt.Sprintf("文件共 %d 行，找不到该片段的起始内容；不要凭记忆改写，先 read_file 确认该处原文。", len(lines))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	return "删除指定文件（不递归删除目录）。"
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
		return tools.Err("%s", errs.FriendlyOr("删除文件", err)), nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return tools.Err("文件不存在: %v", err), nil
	}
	if info.IsDir() {
		return tools.Err("delete_file 不支持删除目录: %s", path), nil
	}
	t.fs.snapshot(ctx, path)
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

// 编译期确认两个写工具都带着先读后写闸门：压缩路径按 tools.ReadGate
// 批量作废登记，漏实现就等于压缩后重新放任凭记忆改写。
var (
	_ tools.ReadGate = (*EditFileTool)(nil)
	_ tools.ReadGate = (*WriteFileTool)(nil)
)

// skipDir 判断是否跳过某些目录（供检索与遍历共用）。
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "vendor", ".idea", ".vscode", "__pycache__", ".codeforge":
		return true
	}
	return false
}
