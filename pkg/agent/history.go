package agent

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
}

// AddUsage 累计一次请求的用量（供上下文统计展示「已使用总 / 缓存命中 / 未命中」）。
func (s *Session) AddUsage(u llm.Usage) {
	if s == nil {
		return
	}
	s.usageIn += u.InputTokens
	s.usageHit += u.CachedTokens
	s.usageOut += u.OutputTokens
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
	}
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
