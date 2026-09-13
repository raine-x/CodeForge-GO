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
type SessionMeta struct {
	ID           string    `json:"id"`
	Workspace    string    `json:"workspace"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
	ArchivedAt   int64     `json:"archived_at"` // 0 = 未归档
}

// Session 是一次完整会话。
type Session struct {
	ID        string
	Workspace string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  []llm.Message
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
			ID:           r.ID,
			Workspace:    r.Workspace,
			Title:        r.Title,
			CreatedAt:    r.CreatedAt,
			UpdatedAt:    r.UpdatedAt,
			MessageCount: r.MessageCount,
			ArchivedAt:   r.ArchivedAt,
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

// RenameWorkspace 重命名工作区分组（批量改写会话归属键）。
func (h *History) RenameWorkspace(oldWS, newWS string) error {
	if err := h.st.RenameWorkspace(oldWS, newWS); err != nil {
		return err
	}
	// 缓存里属于旧键的会话同步改归属
	for _, s := range h.cache {
		if s.Workspace == oldWS {
			s.Workspace = newWS
		}
	}
	return nil
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
