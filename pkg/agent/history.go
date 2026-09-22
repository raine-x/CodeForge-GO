package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"

	"codeforge/pkg/llm"
	"codeforge/pkg/store"
)

// SessionMeta 是会话的轻量元信息。
// WorkspaceName 是所属项目的显示名（未设置时为空串，前端回落到路径末段）。
type SessionMeta struct {
	ID            string    `json:"id"`
	Workspace     string    `json:"workspace"`
	WorkspaceName string    `json:"workspace_name"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	MessageCount  int       `json:"message_count"`
	ArchivedAt    int64     `json:"archived_at"` // 0 = 未归档
}

// Session 是一次完整会话。
type Session struct {
	ID        string
	Workspace string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  []llm.Message

	// ---- 上下文压缩缓存（刻意不持久化：摘要可从 Messages 重新生成）----
	//
	// Messages 永远是完整历史，压缩只影响「送模视图」（见 Agent.requestView）。
	// 把压缩状态放在 Session 上而不是全局 map，生命周期与缓存会话一致，
	// 会话被删除/换出时自动释放。
	//
	// compressedUpTo 表示 Messages[:compressedUpTo] 已被 summaryText 覆盖。
	compressedUpTo int
	summaryText    string

	// ---- 用量统计（进程内存态，不持久化；重启后从零累计）----
	//
	// 由 LLM 适配器在每轮流结束时上报（llm.EventUsage），consumeStream 累加到这里。
	// 放在 Session 上与压缩缓存同理：生命周期跟随缓存会话，删除/换出自动释放。
	usageIn  int // 累计输入 tokens（含缓存命中部分）
	usageHit int // 其中命中上游缓存的 tokens
	usageOut int // 累计输出 tokens

	// reqEstimate 是「上一次真正发给上游的请求」的估算总量
	//（系统提示 + 工具定义 + 消息），用于和上游回报的真实值做比对。
	reqEstimate int

	// tokenFactor 是估算器的**校准系数** = 真实输入 / 估算输入。
	//
	// 为什么需要它：EstimateTokens 对代码/JSON 会低估（代码约 3–3.5 字符/token，
	// 而估算按 4 字符/token 计），压缩线又只留 5% 余量。两者叠加会稳定出现
	// 「判定没超预算、真请求却超窗」—— 2026-09-21 实测：输入 134145 被判为未超，
	// 请求发出后被上游 400 拒绝，整轮任务白跑。
	//
	// 拿到真实值后按比例放大估算，压缩就会在该触发的时候触发
	//（也就是「在合适的时间压缩」，而不是撞墙之后）。
	//
	// 只放大不缩小（下限 1.0）：宁可早压 —— 压缩是幂等的，多压一次只多花一点
	// 摘要成本；漏压的代价是整轮失败。
	tokenFactor float64
}

// AddUsage 累计一次请求的用量（供上下文统计展示「已使用总 / 缓存命中 / 未命中」）。
func (s *Session) AddUsage(u llm.Usage) {
	if s == nil {
		return
	}
	s.usageIn += u.InputTokens
	s.usageHit += u.CachedTokens
	s.usageOut += u.OutputTokens
	s.calibrateTokenFactor(u.InputTokens)
}

// calibrateTokenFactor 用上游回报的真实输入 tokens 校准估算器。
//
// 真实值比估算大多少，后续估算就按同样的比例放大 —— 这样压缩线才真正
// 对应「上游眼里的占用量」，而不是「我们自己算出来的乐观数字」。
func (s *Session) calibrateTokenFactor(realInput int) {
	if s == nil || s.reqEstimate <= 0 || realInput <= 0 {
		return
	}
	f := float64(realInput) / float64(s.reqEstimate)
	if f < 1 {
		f = 1 // 只放大不缩小：宁可早压，别漏压
	}
	if f > maxTokenFactor {
		f = maxTokenFactor
	}
	s.tokenFactor = f
}

// calibratedEstimate 返回按校准系数放大后的估算（无校准数据时等于 EstimateTokens）。
func (s *Session) calibratedEstimate(msgs []llm.Message) int {
	n := EstimateTokens(msgs)
	if s != nil && s.tokenFactor > 1 {
		n = int(math.Round(float64(n) * s.tokenFactor))
	}
	return n
}

// compressionState 返回（是否处于压缩态、被摘要覆盖的消息条数）。
// 越界（历史被 Regenerate 截断或整体替换）时按未压缩处理。
func (s *Session) compressionState() (bool, int) {
	if s == nil || s.compressedUpTo <= 0 || strings.TrimSpace(s.summaryText) == "" {
		return false, 0
	}
	if s.compressedUpTo > len(s.Messages) {
		return false, 0
	}
	return true, s.compressedUpTo
}

// normalizeCompression 自愈压缩状态：历史被回退（Regenerate 截断）或整体替换
// （换会话、测试直接赋值）后，压缩游标可能越过末尾，必须复位，
// 否则 requestView 会切出不存在的区间、或把摘要错误地覆盖到新历史上。
func (s *Session) normalizeCompression() {
	if s == nil {
		return
	}
	if s.compressedUpTo < 0 {
		s.compressedUpTo = 0
	}
	if s.compressedUpTo > len(s.Messages) ||
		(s.compressedUpTo > 0 && strings.TrimSpace(s.summaryText) == "") {
		s.compressedUpTo = 0
		s.summaryText = ""
	}
}

// repairDanglingToolUse 给末尾「没有结果」的工具调用补一条说明性 tool_result。
//
// 只在**最后一条消息**是含 tool_use 的助手消息时才动手 —— 那正是「工具执行到
// 一半进程被杀 / 被强杀」留下的形状。此时会话末尾挂着没有结果的 tool_use，
// 而上游对消息序列有硬约束（每个 tool_use 必须紧跟同 ID 的结果），带着它续跑
// 会被 400 拒绝，用户看到的却只是「模型又报错了」。
//
// 补一条「未执行」的结果，而不是把这条调用删掉：删掉等于连「模型决定做过什么」
// 一起抹了，而续跑恰恰需要它当上下文 —— 2026-09-22 用户投诉的正是「打断后记录
// 被抹掉」，不该在这里制造第二处同类丢失。
//
// 返回补上的工具调用 ID；无需修复时返回 nil。
func (s *Session) repairDanglingToolUse() []string {
	if s == nil || len(s.Messages) == 0 {
		return nil
	}
	last := s.Messages[len(s.Messages)-1]
	if last.Role != llm.RoleAssistant {
		return nil
	}
	// 同一条消息里可能并存正文与多个 tool_use；只有真正没有结果的才算残缺。
	replied := map[string]bool{}
	for _, b := range last.Content {
		if b.Type == llm.BlockToolResult {
			replied[b.ToolUseID] = true
		}
	}
	var fixed []string
	for _, b := range last.Content {
		if b.Type != llm.BlockToolUse || b.ID == "" || replied[b.ID] {
			continue
		}
		fixed = append(fixed, b.ID)
	}
	for _, id := range fixed {
		s.Messages = append(s.Messages, llm.ToolResultMessage(id,
			"（本轮在工具执行完成前被中断，该工具没有产出结果）", true))
	}
	return fixed
}

// History 负责会话历史的持久化（SQLite）与缓存。
//
// workspace 语义：会话按工作区隔离。workspace 为空（未选择工作区）时，
// 会话挂在空串下、只与空工作区互通；切换工作区即切换数据视图。
type History struct {
	st    *store.Store
	cache map[string]*Session
}

// NewHistory 构造会话历史管理器（st 为已打开的 SQLite 存储）。
func NewHistory(st *store.Store) *History {
	return &History{st: st, cache: map[string]*Session{}}
}

// Create 新建一个会话（挂到 workspace 下）。
func (h *History) Create(workspace, title string) (*Session, error) {
	if strings.TrimSpace(title) == "" {
		title = "新会话"
	}
	now := time.Now()
	s := &Session{ID: newID(), Workspace: workspace, Title: title, CreatedAt: now, UpdatedAt: now}
	if err := h.st.CreateSession(s.ID, workspace, s.Title, now); err != nil {
		return nil, err
	}
	h.cache[s.ID] = s
	return s, nil
}

// Get 读取一个会话（优先命中缓存）。
func (h *History) Get(id string) (*Session, bool) {
	if s, ok := h.cache[id]; ok {
		return s, true
	}
	row, ok, err := h.st.GetSession(id)
	if err != nil || !ok {
		return nil, false
	}
	msgs, err := h.st.SessionMessages(id)
	if err != nil {
		return nil, false
	}
	s := &Session{
		ID:        row.ID,
		Workspace: row.Workspace,
		Title:     row.Title,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
		Messages:  msgs,
		// 恢复压缩状态：重启后不该把已摘要覆盖的历史再发一遍。
		// 越界由 normalizeCompression 兜底（历史可能被回退/整体替换过）。
		compressedUpTo: row.CompressedUpTo,
		summaryText:    row.SummaryText,
	}
	s.normalizeCompression()
	h.cache[id] = s
	return s, true
}

// Save 持久化一个会话（内存缓存对象 → SQLite）。
func (h *History) Save(id string) error {
	s, ok := h.cache[id]
	if !ok {
		return fmt.Errorf("会话不存在: %s", id)
	}
	s.UpdatedAt = time.Now()
	if s.Title == "" || s.Title == "新会话" {
		if t := firstUserText(s); t != "" {
			s.Title = truncateRunes(t, 24)
		}
	}
	return h.st.SaveSession(store.SessionRow{
		ID:        s.ID,
		Workspace: s.Workspace,
		Title:     s.Title,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Messages:  s.Messages,
		// 压缩状态一并落盘，否则重启后被摘要覆盖的历史会原样重发、立刻超窗
		CompressedUpTo: s.compressedUpTo,
		SummaryText:    s.summaryText,
	})
}

// List 返回会话元信息（按更新时间倒序）。
// workspace 为空返回全部工作区（侧栏分组视图用）；archived 选择归档与否。
func (h *History) List(workspace string, archived bool) []SessionMeta {
	rows, err := h.st.ListSessions(workspace, archived)
	if err != nil {
		return nil
	}
	metas := make([]SessionMeta, 0, len(rows))
	for _, r := range rows {
		metas = append(metas, SessionMeta{
			ID:            r.ID,
			Workspace:     r.Workspace,
			WorkspaceName: r.WorkspaceName,
			Title:         r.Title,
			CreatedAt:     r.CreatedAt,
			UpdatedAt:     r.UpdatedAt,
			MessageCount:  r.MessageCount,
			ArchivedAt:    r.ArchivedAt,
		})
	}
	return metas
}

// Archive 归档一个会话。
func (h *History) Archive(id string) error {
	delete(h.cache, id) // 归档会话从活跃缓存移除（避免继续续聊）
	return h.st.ArchiveSession(id)
}

// Unarchive 恢复归档的会话。
func (h *History) Unarchive(id string) error { return h.st.UnarchiveSession(id) }

// ArchiveWorkspace 归档整个工作区的会话。
func (h *History) ArchiveWorkspace(workspace string) error {
	return h.st.ArchiveWorkspace(workspace)
}

// UnarchiveWorkspace 恢复整个工作区的会话（与 ArchiveWorkspace 对称）。
// 不动缓存：恢复的会话下次 Get/List 时按需从库里读回来。
func (h *History) UnarchiveWorkspace(workspace string) error {
	return h.st.UnarchiveWorkspace(workspace)
}

// ListWorkspaces 返回全部工作区路径（有未归档会话的）。
func (h *History) ListWorkspaces() []string {
	ws, err := h.st.ListWorkspaces()
	if err != nil {
		return nil
	}
	return ws
}

// Latest 返回工作区最近更新的会话 id（无则空串）。
func (h *History) Latest(workspace string) string {
	id, err := h.st.LatestSession(workspace)
	if err != nil {
		return ""
	}
	return id
}

// Rename 重命名会话。
func (h *History) Rename(id, title string) error {
	if s, ok := h.cache[id]; ok {
		s.Title = title
	}
	return h.st.RenameSession(id, title)
}

// SetWorkspace 更新会话归属的工作区（空工作区会话选中工作区后挂靠）。
func (h *History) SetWorkspace(id, workspace string) error {
	if s, ok := h.cache[id]; ok {
		s.Workspace = workspace
	}
	return h.st.SetSessionWorkspace(id, workspace)
}

// SetWorkspaceName 设置项目的显示名（仅显示用）。
// 刻意不改会话归属键：那个键同时是 agent 的工作目录路径，一改就会让项目「失去工作区」。
func (h *History) SetWorkspaceName(workspace, name string) error {
	return h.st.SetWorkspaceName(workspace, name)
}

// Todos 返回会话的任务清单（按 sort 升序）。无会话返回空数组。
func (h *History) Todos(sessionID string) []store.TodoRow {
	todos, err := h.st.ListTodos(sessionID)
	if err != nil {
		return []store.TodoRow{}
	}
	return todos
}

// ReplaceTodos 整体替换会话的任务清单（todo_write 的整表语义）。
func (h *History) ReplaceTodos(sessionID string, todos []store.TodoRow) error {
	return h.st.ReplaceTodos(sessionID, todos)
}

// WorkspaceName 返回项目的显示名（未设置返回空串）。
func (h *History) WorkspaceName(workspace string) string {
	name, err := h.st.WorkspaceName(workspace)
	if err != nil {
		return ""
	}
	return name
}

// Delete 删除一个会话。
func (h *History) Delete(id string) error {
	delete(h.cache, id)
	return h.st.DeleteSession(id)
}

// DeleteWorkspace 删除整个项目（工作区）下的全部会话，并清掉缓存中属于该项目的会话。
func (h *History) DeleteWorkspace(workspace string) error {
	for id, s := range h.cache {
		if s.Workspace == workspace {
			delete(h.cache, id)
		}
	}
	return h.st.DeleteWorkspace(workspace)
}

// firstUserText 提取首条用户消息文本，用于生成会话标题。
func firstUserText(s *Session) string {
	for _, m := range s.Messages {
		if m.Role != llm.RoleUser {
			continue
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// newID 生成 16 位十六进制随机 ID。
func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
