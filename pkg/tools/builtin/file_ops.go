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
	// SessionID 是写下这次变更的会话。为空表示调用方没带会话信息
	// （进程级入口，比如 HTTP 的撤销接口目前就拿不到）。
	//
	// 有它才能做会话级撤销：undo 栈是进程级的一条直线，多会话并发时
	// 「用户点撤销」会撤掉另一个会话的写入。
	SessionID string `json:"session_id,omitempty"`

	// Spill 是「内容太大时」的落盘副本路径。为空表示内容在 Content 里。
	//
	// 为什么需要：Content 存的是**写前全量内容**，只限条数不限字节的话
	// 最坏情况是 100 × 单文件体积 —— 理论无界。
	Spill string `json:"-"`
	// Dropped 表示内容大到连副本都不值得落（超 maxUndoSpillBytes），
	// 本次改动**不可撤销**。撤销会明确失败而不是拿空内容覆盖文件。
	Dropped bool `json:"-"`
}

// 撤销栈的三个预算。缺一不可：
//
//	maxUndo          条数。界面靠 UndoDepth 展示 remaining，用户对「撤销历史
//	                 是有限的」已有预期。
//	maxUndoBytes     内存总量。这是真正兜住 OOM 的那个上限。
//	maxUndoEntryBytes 单条留在内存里的上限 —— 没有它，一条 2 GB 的快照就能
//	                 独自吃掉整个总预算，逐出会退化成「每次 push 都清空栈」。
//	maxUndoSpillBytes 单个落盘副本的上限 —— 再大就连副本也不落。
const (
	defaultMaxUndo           = 100
	defaultMaxUndoBytes      = 32 << 20 // 32 MiB
	defaultMaxUndoEntryBytes = 1 << 20  // 1 MiB
	defaultMaxUndoSpillBytes = 64 << 20 // 64 MiB
)

// FS 是文件工具共享的工作区状态（含撤销栈）。
type FS struct {
	root         string
	allowOutside bool
	mu           sync.Mutex
	undo         []Snapshot
	// readSeen 是「哪个会话读过/改过哪个文件」的登记表，用于先读后写约束。
	// 整个工作区共用一份 FS，故键必须带上会话 ID：子智能体与各会话之间
	// 不能拿别人读过的原文下自己的笔。
	readSeen map[string]seenEntry
	// pathLocks 给每个规范路径一把互斥锁，用于 CAS 写入的临界区。
	//
	// 用 sync.Map 而非普通 map：casWrite 持路径锁期间还要取 f.mu（读 readSeen），
	// 若 pathLocks 由 f.mu 保护，就形成 f.mu → 路径锁 的顺序；
	// 任何反向顺序都会死锁。sync.Map 的读路径不占全局锁，天然避免。
	pathLocks sync.Map // path → *sync.Mutex

	// undoBytes 是当前撤销栈占用的内存字节（只计 Content，副本在磁盘上）。
	// 与 undo / readSeen 同受 f.mu 守护。
	undoBytes int64

	// 撤销预算与副本目录。四项都可注入，便于测试与将来按需调参。
	maxUndo           int
	maxUndoBytes      int64
	maxUndoEntryBytes int
	maxUndoSpillBytes int
	spillDir          string
}

// SetUndoLimits 覆盖撤销预算（<=0 的项保持现值）。
//
// 存在的理由不只是「可测」：这五个数字是**产品参数**（一个会话该留多少撤销
// 历史），迟早要能按工作区大小或用户偏好调整，而写死在 NewFS 里就没有调整面。
func (f *FS) SetUndoLimits(maxCount int, maxBytes int64, maxEntry, maxSpill int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if maxCount > 0 {
		f.maxUndo = maxCount
	}
	if maxBytes > 0 {
		f.maxUndoBytes = maxBytes
	}
	if maxEntry > 0 {
		f.maxUndoEntryBytes = maxEntry
	}
	if maxSpill > 0 {
		f.maxUndoSpillBytes = maxSpill
	}
}

// SetUndoSpillDir 指定副本目录。测试必须指向临时目录，
// 否则会往真实的 ~/.codeforge/undo/ 里写垃圾。
func (f *FS) SetUndoSpillDir(dir string) {
	f.mu.Lock()
	f.spillDir = dir
	f.mu.Unlock()
}

// undoSpillHome 是默认的副本目录（%USERPROFILE%/.codeforge/undo）。
//
// 刻意不放 <workspace>/.codeforge/：那是用户可见的项目目录，而 FS 是
// **进程级、跨工作区**的（见 AGENTS.md §5 的已知迁移项），快照与工作区
// 没有 1:1 关系。
func undoSpillHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codeforge", "undo")
}

// GCUndoSpill 清空副本目录。
//
// 为什么可以整目录删：进程启动时撤销栈必然是空的 ⇒ 目录里任何文件都是上一次
// 进程（含崩溃）留下的孤儿。前提是单实例 —— cmd/agent/main.go 已在检测到
// 已有实例时拒绝启动。
//
// 刻意**不**做成「NewFS 里自动清」：go test 会并发跑多个测试，
// 一个测试的启动清理会把另一个测试正在用的副本删掉。
func (f *FS) GCUndoSpill() error {
	f.mu.Lock()
	dir := f.spillDir
	f.mu.Unlock()
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// NewFS 构造文件工具工作区。root 为空表示「未选择工作区」，
// 所有文件工具在 选择工作区 之前拒绝执行。
func NewFS(root string) *FS {
	if root = strings.TrimSpace(root); root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	return &FS{
		root:              root,
		maxUndo:           defaultMaxUndo,
		maxUndoBytes:      defaultMaxUndoBytes,
		maxUndoEntryBytes: defaultMaxUndoEntryBytes,
		maxUndoSpillBytes: defaultMaxUndoSpillBytes,
		spillDir:          undoSpillHome(),
		readSeen:          map[string]seenEntry{},
	}
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
	// 副本也要清 —— 否则切工作区就在用户目录里留一堆孤儿。
	for _, s := range f.undo {
		f.removeSpillLocked(s)
	}
	f.undo = nil
	f.undoBytes = 0
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
// snapshot 记录一次「写入前」的状态，并（可选）上报给会话级检查点槽。
//
// before / existed 必须由**调用方**给出，且必须来自 CAS 临界区内读到的那份字节。
// 早先这里是自己 os.ReadFile 一次 —— 那次读发生在写入之后（写路径调整顺序后）
// 或不在临界区内（改造前），记下来的可能是新内容，于是 Undo 变成「再写一遍新内容」。
// 撤销栈记错东西的后果是静默的：撤销看起来成功了，文件却没回到原样。
//
// 两条路径的分工：
//   - 内存 undo 栈：进程内「撤销上一步」，会话结束即失效；
//   - 检查点槽（ctx）：按 (会话, 步骤, 路径) 落库，支持跨重启、按步回滚，
//     是「回退回某条用户消息之前」的基础（Plan.md #4）。
//
// 上报必须发生在写入之前：sink 拿到的是读出的旧内容，晚一步就只能读到新内容。
//
// 返回值是给调用方塞进工具返回值的**人话提示**（见 undoNote）。空串 = 无需提示。
func (f *FS) snapshot(ctx context.Context, path string, before []byte, existed bool) string {
	snap := Snapshot{Path: path, Time: time.Now()}
	// 记下是谁写的，会话级撤销（UndoSession）全靠它。
	if sc, ok := tools.SessionFrom(ctx); ok {
		snap.SessionID = sc.SessionID
	}

	f.mu.Lock()
	maxEntry := f.maxUndoEntryBytes
	maxSpill := f.maxUndoSpillBytes
	spillDir := f.spillDir
	f.mu.Unlock()

	note := ""
	if existed {
		snap.Existed = true
		switch {
		case len(before) <= maxEntry:
			// 内存层：绝大多数编辑走这条路，不产生任何磁盘副���
			snap.Content = before
		case spillDir != "" && len(before) <= maxSpill:
			// 落盘副本层。
			//
			// 这一层不能省：WithCheckpointSink 只在主循环注入，子智能体的写入
			// **内存 undo 栈是唯一的撤销途径**。若对大文件一律降级为哈希，
			// 子智能体改 5 MB 文件就变成不可撤销 —— 那是真的功能回退。
			if p, err := writeSpill(spillDir, before); err == nil {
				snap.Spill = p
			} else {
				snap.Dropped = true
				note = "该文件改动前的内容过大且落盘副本失败，本次改动无法自动撤销。"
			}
			if !snap.Dropped {
				note = "该文件改动前的内容已存到磁盘副本，本次撤销仍然可用。"
			}
		default:
			// 只留指纹层：连副本都不落。
			snap.Dropped = true
			note = "该文件改动前的内容过大，本次改动未保留、无法自动撤销（改动本身已成功）。"
		}
	}

	evicted := f.pushSnapshot(snap)

	if ctx == nil {
		return note
	}
	if sink, ok := tools.CheckpointSinkFrom(ctx); ok {
		sink(tools.CheckpointEvent{
			Path:       path,
			Existed:    snap.Existed,
			OldContent: string(before), // 文件不存在时 before 为 nil，转成空串
		})
	}
	// 逐出也是**有损**事件。常规的「第 101 步挤掉最旧一步」是用户已有预期的
	// 有限历史（界面还展示 remaining），每次都啰嗦是噪音 —— 只在
	// 「因为字节预算被挤掉」时补一句。
	if evicted > 0 && note == "" {
		note = fmt.Sprintf("撤销历史已达上限，本次有 %d 步更早的改动被挤出、无法再撤销。", evicted)
	}
	return note
}

// pushSnapshot 压栈并按两个预算逐出，返回被挤掉的条数。
func (f *FS) pushSnapshot(snap Snapshot) int {
	f.mu.Lock()
	f.undo = append(f.undo, snap)
	f.undoBytes += int64(len(snap.Content))
	evicted := 0
	// 逐出：先按字节预算丢最旧的，再按条数裁剪。
	// 顺序不影响正确性，但字节优先 —— 它才是真正兜住 OOM 的那个上限。
	for len(f.undo) > 0 && (f.undoBytes > f.maxUndoBytes || len(f.undo) > f.maxUndo) {
		// 绝不逐出栈顶：Undo() 撤的是最后一条，逐出它等于让「撤销上一步」失效。
		if len(f.undo) == 1 && f.undoBytes > f.maxUndoBytes {
			break
		}
		old := f.undo[0]
		f.undoBytes -= int64(len(old.Content))
		if f.undoBytes < 0 {
			f.undoBytes = 0
		}
		f.undo = f.undo[1:]
		f.removeSpillLocked(old)
		evicted++
	}
	f.mu.Unlock()
	return evicted
}

// writeSpill 把内容写成临时副本，返回路径。
//
// 用 os.CreateTemp 的唯一名而不是内容寻址：同一份内容被两条快照引用时，
// 内容寻址要删一条就会连带删掉另一条还在用的文件，要正确处理就得引入 refcount。
// 唯一名 + 无脑 Remove 才是可证明正确的最小实现。
func writeSpill(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	fh, err := os.CreateTemp(dir, "undo-*.bak")
	if err != nil {
		return "", err
	}
	name := fh.Name()
	if _, err := fh.Write(data); err != nil {
		fh.Close()
		os.Remove(name)
		return "", err
	}
	if err := fh.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// removeSpillLocked 删掉快照的副本。调用方必须已持 f.mu。
func (f *FS) removeSpillLocked(s Snapshot) {
	if s.Spill != "" {
		_ = os.Remove(s.Spill)
	}
}

// Undo 撤销最近一次文件写入，返回被还原的路径。
//
// 进程级语义：不管是谁写的，撤最后一条。HTTP 的 handleUndo 目前就靠它
// （前端没有 session header，拿不到会话）。
func (f *FS) Undo() (string, bool) {
	return f.undoAt(-1)
}

// UndoSession 撤销指定会话最近一次写入。
//
// 会话级语义：只动该会话自己的写入，别的会话的栈条目原样留着。
// 从后往前找第一个 SessionID 匹配的条目。
func (f *FS) UndoSession(sessionID string) (string, bool) {
	f.mu.Lock()
	idx := -1
	for i := len(f.undo) - 1; i >= 0; i-- {
		if f.undo[i].SessionID == sessionID {
			idx = i
			break
		}
	}
	f.mu.Unlock()
	if idx < 0 {
		return "", false
	}
	return f.undoAt(idx)
}

// Snapshots 返回当前撤销栈的副本（自旧到新），供诊断与测试使用。
func (f *FS) Snapshots() []Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Snapshot, len(f.undo))
	copy(out, f.undo)
	return out
}

// undoAt 撤销栈中第 idx 条（-1 = 最后一条）。
//
// 撤销会把文件内容**改回**旧值，因此所有相关会话的指纹都必须一起刷新 ——
// 否则「读 → 写 → 撤销 → 再写」会在最后一步被判成「被外部修改」而失败。
//
// ⚠️ **降级条目（Dropped）必须走「删登记」而不是「按空内容算指纹」。**
// 留着旧指纹的话，下一次写入会基于一个错误的基线通过 CAS，
// 整个指纹机制就被绕过了。有测试专门盯这条。
//
// 调用方**不要**持有 f.mu：本方法自己加锁。
func (f *FS) undoAt(idx int) (string, bool) {
	f.mu.Lock()
	if idx < 0 {
		idx = len(f.undo) + idx
	}
	if idx < 0 || idx >= len(f.undo) {
		f.mu.Unlock()
		return "", false
	}
	snap := f.undo[idx]
	f.undo = append(f.undo[:idx:idx], f.undo[idx+1:]...)
	f.undoBytes -= int64(len(snap.Content))
	if f.undoBytes < 0 {
		f.undoBytes = 0
	}
	// ⚠️ 副本必须**先读完再删**。反过来的话这里读到的是不存在的文件，
	// 撤销会静默失败，而「有副本却撤不了」是最难查的一种坏。
	//
	// 读在锁内做：副本可能正被另一个 goroutine 的逐出删除。
	var spillRaw []byte
	if snap.Spill != "" {
		spillRaw, _ = os.ReadFile(snap.Spill)
	}
	f.removeSpillLocked(snap)
	f.mu.Unlock()

	// 完全被丢弃（Dropped）的条目**没有内容可用**：明确失败，
	// 绝不拿空内容去覆盖文件 —— 那是最灾难性的数据损坏。
	content := snap.Content
	if snap.Spill != "" {
		if spillRaw == nil {
			// 副本丢了：同样只能失败。
			return snap.Path, false
		}
		content = spillRaw
	}
	// 内容可能被丢弃 —— 但**指纹必须先作废**，然后才能放弃。
	//
	// 顺序很重要：若在这里就 return，那些指向旧基线的指纹会留在 readSeen 里，
	// 下一次写入会基于一个错误的基线通过 CAS，整个指纹机制被绕过。
	// 有测试专门盯这条（TestDroppedSnapshotInvalidatesFingerprint）。
	restorable := !snap.Dropped || content != nil

	// 所有见过这个路径的会话，其指纹都指向「改动前」的内容。
	// 撤销把它们统一对齐到还原后的内容。
	// seenKey 的结构是 sessionID + "\n" + path，所以匹配后缀时要带前导 \n，
	// 否则路径 "a.go" 会误匹配到 "xa.go"。
	suffix := "\n" + snap.Path
	restored := snap.Existed && restorable
	var sum string
	var size int64
	if restored {
		sum, size = fingerprint(content)
	}
	f.mu.Lock()
	for k := range f.readSeen {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		if restored {
			f.readSeen[k] = seenEntry{sum: sum, size: size}
		} else {
			// 文件被撤销成「原本不存在」，**或**内容已不可得（Dropped）——
			// 两种情况指纹都无从谈起。删掉登记，后续写入会走「新建文件」路径
			// （casCheckReadGate 对不存在的文件放行）或要求重读。
			// 绝不能留着一个指向旧基线的指纹。
			delete(f.readSeen, k)
		}
	}
	f.mu.Unlock()

	if !restorable {
		// 指纹已作废，这里才安全地放弃：文件内容不可得，明确失败，
		// 绝不拿空内容去覆盖 —— 那是最灾难性的数据损坏。
		return snap.Path, false
	}

	if snap.Existed {
		_ = os.MkdirAll(filepath.Dir(snap.Path), 0o755)
		if err := os.WriteFile(snap.Path, content, 0o644); err != nil {
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
// ---------------------------------------------------------------------------
// CAS 写入：把「读当前 → 比对 → 写」收进同一把按路径的锁
// ---------------------------------------------------------------------------

// lockPath 取出某路径的互斥锁，首次调用时创建。
//
// 为什么不用一张普通 map：需要一个「按 key 懒创建」的结构，而 mutex 本身
// 会被调用方持有，**不能**把 f.mu 一起带上（否则持路径锁时再取 f.mu
// 就有死锁风险）。所以用 sync.Map —— 它的读路径不占全局锁。
func (f *FS) lockPath(path string) *sync.Mutex {
	v, _ := f.pathLocks.LoadOrStore(path, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// casWrite 在「内容仍与调用方看到的一致」的前提下原子写入，返回写入前的内容。
//
// existed 是调用方**自己**的判断（true = 它认为自己覆盖的是个已有文件，
// false = 它认为自己是在新建）。这个参数不是多余的：新建路径上同样有
// 竞态 —— 两个会话都认为「文件不存在」而同时创建，后者会静默覆盖前者。
//
// 返回值是「写入前的实际内容」，让调用方做改动行数统计时不必再读一次盘
// —— 那些读已经不在临界区里，值可能过期。
func (f *FS) casWrite(ctx context.Context, path string, existed bool, next []byte) ([]byte, error) {
	mu := f.lockPath(path)
	mu.Lock()
	defer mu.Unlock()

	// —— 临界区：读当前 ——
	cur, readErr := os.ReadFile(path)
	nowExists := readErr == nil

	if nowExists != existed {
		if existed {
			return nil, tools.NewErrStaleContent(path,
				"写入时发现该文件已不存在（可能刚被删除或重命名）。"+
					"请重新 read_file 确认现状，再基于它提交修改。")
		}
		return nil, tools.NewErrStaleContent(path,
			"写入时发现该文件已经存在 —— 它的存在不是你这次读到的状态。"+
				"请先 read_file 看看现在的内容，确认是要覆盖它还是换用别的路径。")
	}

	// 前置条件：读门禁。未读过 / 指纹不符 → 拒绝。
	if err := f.casCheckReadGate(ctx, path, cur, existed); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := atomicWriteFile(path, next, 0o644); err != nil {
		return nil, err
	}
	return cur, nil
}

// casCheckReadGate 是 CAS 内部的前置条件检查，**必须在临界区内调用**。
//
// 拆成独立函数是为了让「锁的边界」在代码里一目了然：casWrite 里读与写之间
// 只允许出现这个纯检查，不能再有别的读盘或可能失败的 IO。
func (f *FS) casCheckReadGate(ctx context.Context, path string, cur []byte, existed bool) error {
	// 新建：没有可丢的旧内容，不拦读门禁（与改造前 requireReadSeen 对
	// !existed 直接返回 nil 一致）。
	//
	// 「仍不存在」这件事 casWrite 已经断言过了 —— 那是这条路径上真正有价值的
	// 新增防护（挡住两个会话同时创建同一文件）。
	if !existed {
		return nil
	}
	// 以下只针对已存在的文件。
	//
	// 两种情形仍不拦：
	//   - 调用不在会话运行域内：无法判定「谁读过」，此时拦截只会让工具不可用。
	sc, ok := tools.SessionFrom(ctx)
	if !ok || sc.SessionID == "" {
		return nil
	}
	f.mu.Lock()
	entry, seen := f.readSeen[seenKey(sc.SessionID, path)]
	f.mu.Unlock()

	if !seen {
		return tools.NewErrStaleContent(path,
			"本会话还没读过这个文件，不能凭记忆改写。先用 read_file 读取"+
				"（大文件按返回末尾的行号窗口分段读），看到原文后再提交精确替换。")
	}
	if entry.matches(fingerprint(cur)) {
		return nil
	}
	return tools.NewErrStaleContent(path,
		"在本会话读取之后被外部修改过"+sizeDelta(entry.size, int64(len(cur)))+"。"+
			"请先重新 read_file 看到最新内容，再基于它提交修改 —— "+
			"否则会覆盖掉这段时间别人做的改动。")
}

// atomicWriteFile 用「同目录临时文件 + rename」落盘。
//
// 为什么不直接 os.WriteFile：WriteFile 是「打开 → 截断 → 写」，
// 进程在写一半被杀（OOM、用户 Ctrl+C、崩溃）会留下**半截文件**。
// rename 在同一文件系统内是原子的，读者只会看到完整的旧内容或完整的新内容。
//
// 同目录是必须的：跨文件系统 rename 会失败。
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".cf-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 任何失败路径都要清掉临时文件，否则目录里会积累垃圾
	// （这也是「落盘后目录里多了残留文件」那条测试要守的东西）。
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp 用 0600 建文件；显式改成目标权限，否则产物权限会莫名变严。
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // 已消费，defer 不再删
	return nil
}

// requireReadSeen 保留给只需要「检查」而不需要「写入」的场景。
//
// 写路径**不再**用它 —— 那是 CAS 之前的做法，窗口太大。
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

// Metadata 声明副作用等级。
func (t *ReadFileTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}

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

// Metadata 声明副作用等级。
func (t *ListDirTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}

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

// Metadata 声明副作用等级。
func (t *WriteFileTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectWrite}
}

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
	// 这里只需要知道「目标是否已存在」。刻意不取内容：CAS 内部会在临界区里
	// 读一次并把旧内容一并返回，早先这里再读一份纯属浪费，且那次读不在临界区
	// 里、值可能已经过期。
	//
	// 用 ReadFile 而非 Stat 探测存在性：目标是目录时 Stat 会成功，
	// 而 ReadFile 失败 —— 后者才是「不是可写文件」的正确判据。
	_, readErr := os.ReadFile(path)
	existed := readErr == nil
	// 全部收进同一把按路径的锁里：读当前 → 比对 → 原子写。
	// 返回的 before 是临界区内读到的真实旧内容，直接用于改动行数统计 ——
	// 早先这里另开一次 os.ReadFile，那次读已经不在临界区里，值可能过期。
	before, casErr := t.fs.casWrite(ctx, path, existed, []byte(content))
	if casErr != nil {
		return tools.Err("%s", casErr), nil
	}
	undoNote := t.fs.snapshot(ctx, path, before, existed)
	t.fs.markReadSeen(ctx, path, []byte(content))
	out := map[string]any{"path": path, "bytes": len(content), "created": !existed}
	if undoNote != "" {
		out["undo_note"] = undoNote
	}
	if existed {
		added, removed := LineChurn(string(before), content)
		out["added"], out["removed"] = added, removed
		out["lines_before"], out["lines_after"] = len(splitLines(string(before))), len(splitLines(content))
		if hint := overwriteHint(string(before), content, added, removed); hint != "" {
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

// casEdit 在同一把路径锁内完成「读当前 → 校验 → 替换 → 原子写」。
//
// 为什么不能拆成「读 → 算 updated → 写」三步（改造前就是这样）：
// updated 是从**读到的内容**算出来的，而写发生在之后。若中途有别人改动，
// 我们会拿着基于旧内容算出的结果去覆盖 —— 而且 applyPairs 很可能匹配失败
// 或匹配到错误位置。校验放在写之前只能证明「写之前没变过」，不能证明
// 「我算 updated 时看到的就是我要覆盖的那份」。
//
// 收进一个临界区后，「我编辑的对象」与「我校验的对象」是同一份字节。
func (f *FS) casEdit(ctx context.Context, path string, pairs []editPair) (updated string, count int, before []byte, err error) {
	mu := f.lockPath(path)
	mu.Lock()
	defer mu.Unlock()

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return "", 0, nil, readErr
	}
	// edit_file 只处理已存在的文件；这里要求 existed=true，
	// 顺带把「文件刚被别人删了」也纳入同一套拒绝逻辑。
	if err := f.casCheckReadGate(ctx, path, data, true); err != nil {
		return "", 0, nil, err
	}
	updated, count, err = applyPairs(string(data), pairs)
	if err != nil {
		return "", 0, nil, err
	}
	if err := atomicWriteFile(path, []byte(updated), 0o644); err != nil {
		return "", 0, nil, err
	}
	return updated, count, data, nil
}

// casRemove 在同一把路径锁内完成「确认存在 → 校验读门禁 → 快照 → 删除」。
//
// 改造前是 os.Stat 之后 os.Remove，中间有窗口；而且**完全没有读门禁** ——
// 删一个没读过的文件与覆盖它是同一类丢数据，且更不可逆。
func (f *FS) casRemove(ctx context.Context, path string) error {
	mu := f.lockPath(path)
	mu.Lock()
	defer mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		return tools.NewErrStaleContent(path,
			"删除时发现该文件已不存在（可能刚被别人删了或重命名）。请先 read_file 确认现状。")
	}
	if info.IsDir() {
		return fmt.Errorf("delete_file 不支持删除目录: %s", path)
	}
	cur, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("无法确认 %s 的内容：%w", path, err)
	}
	if err := f.casCheckReadGate(ctx, path, cur, true); err != nil {
		return err
	}
	// 快照必须在删除前记，且记的是刚在锁内读到的内容。
	f.snapshot(ctx, path, cur, true)
	if err := os.Remove(path); err != nil {
		return err
	}
	return nil
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

// Metadata 声明副作用等级。
func (t *EditFileTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectWrite}
}

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
	// 读、校验、替换、写全部在 casEdit 的一次临界区内完成。
	// 改造前是「先 os.ReadFile → applyPairs → os.WriteFile」三步裸奔，
	// updated 基于旧内容算出、写入却无保护。
	updated, count, data, casErr := t.fs.casEdit(ctx, path, pairs)
	if casErr != nil {
		return tools.Err("%s", casErr), nil
	}
	undoNote := t.fs.snapshot(ctx, path, data, true)
	t.fs.markReadSeen(ctx, path, []byte(updated))
	if undoNote != "" {
		return tools.OkMeta(map[string]any{
			"path":         path,
			"hunks":        len(pairs),
			"replacements": count,
			"undo_note":    undoNote,
		}, map[string]any{"path": path}), nil
	}
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

// Metadata 声明副作用等级。
func (t *DeleteFileTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectDestructive}
}

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
	if err := t.fs.casRemove(ctx, path); err != nil {
		return tools.Err("%s", err), nil
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
