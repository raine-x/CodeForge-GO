// checkpoints.go 实现「检查点 / 回滚」：把写工具执行前的文件快照按
// (会话, 步骤, 路径) 记入 SQLite，并支持按步骤把文件恢复到当时的样子。
//
// 为什么需要它：会话历史可以截断重来（Regenerate / 编辑重发），但磁盘上被
// 改过的文件不会自己回去。没有这一层，「重新生成」之后模型看到的代码与它
// 以为的上下文就对不上了 —— 越是多步写入的任务，错得越离谱。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"codeforge/pkg/logx"
	"codeforge/pkg/platform"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// CheckpointStep 是一个回滚点（供前端列表展示）。
type CheckpointStep struct {
	Step   int    `json:"step"`   // 步骤号
	Files  int    `json:"files"`  // 该步骤改动的文件数
	At     int64  `json:"at"`     // 最近一次改动时间（unix 秒）
	Sample string `json:"sample"` // 首个被改动文件的路径
}

// RewindResult 是一次回滚的结果。
type RewindResult struct {
	ToStep   int      `json:"to_step"`  // 回滚到哪一步之前
	Restored int      `json:"restored"` // 成功写回的文件数
	Deleted  int      `json:"deleted"`  // 成功删除的文件数（回滚前并不存在）
	Failed   int      `json:"failed"`   // 失败数
	Paths    []string `json:"paths"`    // 涉及的文件路径（含失败项）
	Errors   []string `json:"errors,omitempty"`
}

// recordCheckpoint 把一次文件写入前的快照落库（由 runLoop 注入的 sink 调用）。
//
// 去重交给数据库主键（session_id, step, path）+ INSERT OR IGNORE：
// 同一步内反复写同一个文件只保留第一次的旧内容，那才是「这一步之前」的样子。
// 落库失败只记日志不打断工具执行 —— 检查点是增强能力，不该让写入本身失败。
func (a *Agent) recordCheckpoint(sessionID string, step int, ev tools.CheckpointEvent) {
	if a.memoryStore == nil || strings.TrimSpace(ev.Path) == "" {
		return
	}
	err := a.memoryStore.InsertCheckpoint(sessionID, store.CheckpointRow{
		Step:       step,
		Path:       ev.Path,
		Existed:    ev.Existed,
		OldContent: ev.OldContent,
	})
	if err != nil {
		logx.Errorf("会话=%s 步骤=%d 记录失败（路径=%s）：%v", sessionID, step, ev.Path, err)
	}
}

// RecordCheckpoint 公开版本的检查点记录（供测试与外部显式造点使用）。
// 生产链路走 recordCheckpoint（由 runLoop 注入的 sink 调用），语义完全一致。
func (a *Agent) RecordCheckpoint(sessionID string, step int, path string, existed bool, oldContent string) {
	a.recordCheckpoint(sessionID, step, tools.CheckpointEvent{
		Path:       path,
		Existed:    existed,
		OldContent: oldContent,
	})
}

// CheckpointSteps 返回会话的全部回滚点（步骤倒序）。
func (a *Agent) CheckpointSteps(sessionID string) []CheckpointStep {
	if a.memoryStore == nil {
		return nil
	}
	rows, err := a.memoryStore.ListCheckpointSteps(sessionID)
	if err != nil {
		return nil
	}
	out := make([]CheckpointStep, 0, len(rows))
	for _, r := range rows {
		out = append(out, CheckpointStep{
			Step:   r.Step,
			Files:  r.Files,
			At:     r.At.Unix(),
			Sample: r.Sample,
		})
	}
	return out
}

// CheckpointFor 返回某会话某路径的检查点，供「这条改动到底改了什么」的查看。
//
// step >= 0：取该步骤上的那一条（实时卡片知道自己的步骤号，最精确）；
// step < 0 ：取该路径**最早**的一条 —— 也就是「这个文件在本次会话里被第一次改动
// 之前是什么样」。历史回放场景前端拿不到步骤号，用它与当前文件对比，
// 得到的是累计改动（接口会注明对比基准，不假装是当时那一次编辑）。
//
// 只按会话内**已记录**的路径查找：查不到就不给 diff —— 这样这个读取口
// 天然被限制在「本次会话确实改过」的文件上，不会退化成任意路径读取。
//
// ⚠️ 路径按**三级容错**匹配（见 pathMatchers）。原因：前端回放卡片带的是
// **模型传入的原始路径**（历史里存的也是原始 Input），而检查点存的是
// FS.Resolve 之后的**绝对路径**。工具 schema 明确允许相对路径，于是
// `pkg/x.go`、`./pkg/x.go`、Windows 下大小写不同的写法全都精确匹配不上 ——
// 点开只会得到一句「可能已被回退」的假话。
//
// 容错**不放宽安全边界**：三级都只在「该会话已记录的路径」里找，
// 绝不会去读一个没被改过的文件。
func (a *Agent) CheckpointFor(sessionID, path string, step int) (store.CheckpointRow, bool) {
	if a.memoryStore == nil || strings.TrimSpace(path) == "" {
		return store.CheckpointRow{}, false
	}
	rows, err := a.memoryStore.ListCheckpoints(sessionID)
	if err != nil {
		return store.CheckpointRow{}, false
	}
	// 逐级放宽：精确 → 绝对化 → 边界后缀。命中即止（后一级更宽松，容易选错文件）。
	for _, match := range pathMatchers(path, a.WorkDir()) {
		if row, ok := pickCheckpoint(rows, match, step); ok {
			return row, true
		}
	}
	return store.CheckpointRow{}, false
}

// pickCheckpoint 在 rows 里按 match 选一行：指定 step 就精确到那一步，
// 否则取该路径**最早**的一条（累计改动的对比基准）。
func pickCheckpoint(rows []store.CheckpointRow, match func(string) bool, step int) (store.CheckpointRow, bool) {
	var earliest store.CheckpointRow
	found := false
	var distinct string // 后缀匹配时记录命中的**不同**存储路径，用于发现歧义
	ambiguous := false
	for _, r := range rows {
		if !match(r.Path) {
			continue
		}
		if step >= 0 {
			if r.Step != step {
				continue
			}
			// ⚠️ 这里曾经直接 return，从不检查歧义 ——
			// 于是「<root>/x/y/f.go」与「<root>/z/y/f.go」都记在同一步时，
			// 查后缀 + step 会静默给出其中一条。
			// 第 ③ 级改成平台化大小写之后，「大小写不同」与「同后缀不同根」
			// 这两类歧义在带 step 的查询上更容易命中，只查 step<0 等于修一半。
			if distinct == "" {
				distinct = r.Path
			} else if distinct != r.Path {
				ambiguous = true
			}
			if !found || r.Step < earliest.Step {
				earliest, found = r, true
			}
			continue
		}
		if !found || r.Step < earliest.Step {
			earliest, found = r, true
		}
		if distinct == "" {
			distinct = r.Path
		} else if distinct != r.Path {
			// 同一个后缀命中了两个不同的文件：宁可报「没匹配到」也不给一份
			// 张冠李戴的 diff（用户看不出那是另一个文件）。
			return store.CheckpointRow{}, false
		}
	}
	if ambiguous {
		return store.CheckpointRow{}, false
	}
	return earliest, found
}

// caseInsensitiveOS 报告当前平台的文件系统是否不区分大小写。
//
// 只有 Windows 是 false —— Termux/Android 的 ext4 与 Linux 一样区分大小写。
// 写 runtime.GOOS 而不用 OSName() 会漏掉 Termux 这个交叉编译目标。
func caseInsensitiveOS() bool { return platform.OSName() == "windows" }

// pathMatchers 按「由严到松」返回三级路径匹配器。
//
//	① 精确（Clean 后相等）
//	② 绝对化：相对路径按工作区根展开 —— 覆盖 `pkg/x.go`、`./pkg/x.go`
//	③ 边界后缀：覆盖分隔符差异，以及**大小写不敏感平台上的**大小写差异。
//	   必须落在分隔符边界上，否则 `partA` 会误配 `partA2`（与 scopesOverlap 同一口径）。
//
// 第 ③ 级的大小写折叠**只在 Windows 开启**：在区分大小写的文件系统上，
// 忽略大小写会把两个**不同的**文件判成同一个 —— Linux 上 `Pkg/x.go` 与
// `pkg/x.go` 是两个文件，回放卡片点开会拿到另一个文件的 diff。
// 代价是 macOS（默认 APFS 实际不区分）会漏配，走「没匹配到本会话的改动记录」
// 这句实话；方向是对的 —— fail-closed（查不到）远好于 fail-open（给错 diff）。
func pathMatchers(path, root string) []func(string) bool {
	return pathMatchersCase(path, root, caseInsensitiveOS())
}

// pathMatchersCase 是可注入大小写策略的版本，供测试在任意平台上验证两种语义。
//
// 抽出来是有必要的：第 ③ 级的两种语义天然互斥，只按当前平台生效的话，
// 开发机（Windows）上永远测不到「区分大小写时不该匹配」这一侧，
// 而那恰恰是这次修的 bug。
func pathMatchersCase(path, root string, caseInsensitive bool) []func(string) bool {
	want := filepath.Clean(path)
	out := []func(string) bool{
		func(p string) bool { return p == want },
	}
	if !filepath.IsAbs(want) && root != "" {
		abs := filepath.Clean(filepath.Join(root, want))
		out = append(out, func(p string) bool { return p == abs })
	}
	norm := func(s string) string {
		s = filepath.ToSlash(s)
		if caseInsensitive {
			return strings.ToLower(s)
		}
		return s
	}
	key := norm(want)
	out = append(out, func(p string) bool {
		lp := norm(p)
		return lp == key || strings.HasSuffix(lp, "/"+key)
	})
	return out
}

// RewindFiles 把工作区文件恢复到「第 toStep 步开始之前」的样子。
//
// 逐条按步骤倒序恢复，而不是「每个路径取最早的快照写一次」：
// 同一个文件可能在多步里被改写，倒序逐条回放才能保证最终落到最早的旧内容，
// 中途若某一步失败，前面已恢复的部分仍是对的（幂等，可重试）。
//
// 成功恢复后，toStep 及之后的检查点会被删除 —— 它们描述的是已被撤销的写入，
// 留着会让下一次回滚把刚还原的文件又写回旧内容。
func (a *Agent) RewindFiles(sessionID string, toStep int) (*RewindResult, error) {
	if a.memoryStore == nil {
		return nil, fmt.Errorf("存储未就绪，无法回滚")
	}
	rows, err := a.memoryStore.CheckpointsFrom(sessionID, toStep)
	if err != nil {
		return nil, fmt.Errorf("读取检查点失败: %w", err)
	}

	res := &RewindResult{ToStep: toStep, Paths: []string{}}
	seen := map[string]bool{}
	for _, r := range rows {
		if !seen[r.Path] {
			seen[r.Path] = true
			res.Paths = append(res.Paths, r.Path)
		}
		if err := restoreFile(r); err != nil {
			res.Failed++
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", r.Path, err))
			continue
		}
		if r.Existed {
			res.Restored++
		} else {
			res.Deleted++
		}
	}
	sort.Strings(res.Paths)

	// 全部成功才清理检查点：有失败项时保留，方便用户修正后重试。
	if res.Failed == 0 && len(res.Paths) > 0 {
		if _, err := a.memoryStore.DeleteCheckpointsFrom(sessionID, toStep); err != nil {
			logx.Errorf("会话=%s 回滚后清理检查点失败：%v", sessionID, err)
		}
	}
	return res, nil
}

// restoreFile 把单个文件恢复到快照状态。
func restoreFile(r store.CheckpointRow) error {
	if r.Existed {
		if dir := filepath.Dir(r.Path); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("创建目录失败: %w", err)
			}
		}
		if err := os.WriteFile(r.Path, []byte(r.OldContent), 0o644); err != nil {
			return fmt.Errorf("写回失败: %w", err)
		}
		return nil
	}
	// 写入前并不存在 ⇒ 回滚 = 删掉它。文件已经不在了（用户手动删过）也算成功。
	if err := os.Remove(r.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除失败: %w", err)
	}
	return nil
}

// lastCheckpointStep 返回会话已记录的最大步骤号（无检查点返回 0）。
// 「回退某条用户消息之后的所有更改」用它划出要回滚的步骤区间。
func (a *Agent) lastCheckpointStep(sessionID string) int {
	steps := a.CheckpointSteps(sessionID)
	if len(steps) == 0 {
		return 0
	}
	max := 0
	for _, s := range steps {
		if s.Step > max {
			max = s.Step
		}
	}
	return max
}

// RewindAfterEdit 在「编辑重发」时回退被编辑消息之后的文件改动。
//
// 步骤归属的精确对齐：检查点步骤号会话级单调（stepBase + 本轮第几步，见
// runLoopWithLimit），每完成一步恰好追加一条助手消息，因此
// 「目标用户消息之前的助手消息数 + 1」就是其后第一个新步骤号。
// steer 注入的用户消息不占步骤 —— 旧的 (dropped+1)/2 近似口径会被它带偏。
//
// 保守起见仍取**最大**匹配：从 idx 之后的第一个步骤起全部回退。
// 宁可多退（用户能重跑）也不要少退（留下与上下文矛盾的半成品）。
//
// 无法定位目标消息（back 越界）时返回 (nil, nil)：只截断对话，不动文件。
func (a *Agent) RewindAfterEdit(sessionID string, back int) (*RewindResult, error) {
	sess, ok := a.history.Get(sessionID)
	if !ok {
		return nil, fmt.Errorf("会话不存在: %s", sessionID)
	}
	if back < 0 {
		back = 0
	}
	// 编辑重发由 WS dispatch 触发，可能与运行中的循环并发：读历史要持锁。
	sess.mu.RLock()
	// 与 EditableUserMessages / RerunFrom 同源：back 只数「真正的提问」，
	// 否则同一个 back 在「定位消息」与「回退文件」两处会指向不同的消息。
	idx := nthLastUserQuestionIndex(sess.Messages, back)
	if idx < 0 {
		sess.mu.RUnlock()
		return nil, nil // 定位不到：跳过文件回滚
	}
	if len(sess.Messages)-(idx+1) <= 0 {
		sess.mu.RUnlock()
		return nil, nil // 目标消息就是最后一条，其后没有改动
	}
	from := countAssistantMessages(sess.Messages[:idx+1]) + 1
	sess.mu.RUnlock()
	maxStep := a.lastCheckpointStep(sessionID)
	if maxStep <= 0 || from > maxStep {
		return nil, nil // 该消息之后的轮次没有记录过写操作
	}
	return a.RewindFiles(sessionID, from)
}
