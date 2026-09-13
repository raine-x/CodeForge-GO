package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codeforge/pkg/llm"
)

// SessionRow 是会话+首条消息的组装结果（对外即一个完整会话）。
type SessionRow struct {
	ID        string
	Workspace string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	Messages  []llm.Message
}

// SessionMetaRow 是会话元信息。
type SessionMetaRow struct {
	ID           string    `json:"id"`
	Workspace    string    `json:"workspace"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	MessageCount int       `json:"message_count"`
	ArchivedAt   int64     `json:"archived_at"` // 0 = 未归档；>0 = 归档时间戳（unix 秒）
}

// CreateSession 新建会话（无消息）。
func (s *Store) CreateSession(id, workspace, title string, now time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, workspace, title, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, workspace, title, now.Unix(), now.Unix())
	return err
}

// GetSession 读取会话元信息（不含消息）。
func (s *Store) GetSession(id string) (*SessionRow, bool, error) {
	row := s.db.QueryRow(`SELECT id, workspace, title, created_at, updated_at FROM sessions WHERE id = ?`, id)
	var r SessionRow
	var created, updated int64
	if err := row.Scan(&r.ID, &r.Workspace, &r.Title, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	r.CreatedAt = time.Unix(created, 0)
	r.UpdatedAt = time.Unix(updated, 0)
	return &r, true, nil
}

// SaveSession 整体写入会话（元信息 + 全量消息替换）。
// 调用方保证 id 已存在（Create 先行）；消息按 seq 全删全写，实现简单且天然幂等。
func (s *Store) SaveSession(sess SessionRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		sess.Title, sess.UpdatedAt.Unix(), sess.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sess.ID); err != nil {
		return err
	}
	for i, m := range sess.Messages {
		content, err := json.Marshal(m.Content)
		if err != nil {
			return fmt.Errorf("序列化消息内容失败: %w", err)
		}
		if _, err := tx.Exec(
			`INSERT INTO messages (session_id, seq, role, content) VALUES (?, ?, ?, ?)`,
			sess.ID, i, string(m.Role), string(content)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SessionMessages 读取会话的全部消息（按 seq 升序）。
func (s *Store) SessionMessages(id string) ([]llm.Message, error) {
	rows, err := s.db.Query(`SELECT role, content FROM messages WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []llm.Message
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		var blocks []llm.ContentBlock
		if err := json.Unmarshal([]byte(content), &blocks); err != nil {
			// 单条消息损坏不应拖垮整个会话：跳过并保留其余
			continue
		}
		msgs = append(msgs, llm.Message{Role: llm.Role(role), Content: blocks})
	}
	return msgs, rows.Err()
}

// ListSessions 列出会话元信息（更新时间倒序）。
// workspace 为空返回全部工作区；archived=false 只返回未归档，true 只返回已归档。
func (s *Store) ListSessions(workspace string, archived bool) ([]SessionMetaRow, error) {
	q := `SELECT s.id, s.workspace, s.title, s.created_at, s.updated_at, s.archived_at,
	      (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id) AS cnt
	      FROM sessions s WHERE s.archived_at ` + opArchived(archived)
	var args []any
	if strings.TrimSpace(workspace) != "" {
		q += ` AND s.workspace = ?`
		args = append(args, workspace)
	}
	q += ` ORDER BY s.updated_at DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	metas := make([]SessionMetaRow, 0, 16)
	for rows.Next() {
		var m SessionMetaRow
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.Workspace, &m.Title, &created, &updated, &m.ArchivedAt, &m.MessageCount); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0)
		m.UpdatedAt = time.Unix(updated, 0)
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

func opArchived(archived bool) string {
	if archived {
		return `> 0`
	}
	return `= 0`
}

// ArchiveSession 归档一个会话（记录归档时间，10 天后由清理任务删除）。
func (s *Store) ArchiveSession(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET archived_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// UnarchiveSession 恢复归档的会话。
func (s *Store) UnarchiveSession(id string) error {
	_, err := s.db.Exec(`UPDATE sessions SET archived_at = 0 WHERE id = ?`, id)
	return err
}

// ArchiveWorkspace 把整个工作区的全部会话归档。
func (s *Store) ArchiveWorkspace(workspace string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET archived_at = ? WHERE workspace = ? AND archived_at = 0`,
		time.Now().Unix(), workspace)
	return err
}

// RenameWorkspace 重命名工作区：仅改分组显示名（工作区路径本身不可改，
// 分组显示名存储见 workspace_names 表；这里同步改会话行的归属展示键）。
func (s *Store) RenameWorkspace(oldWS, newWS string) error {
	_, err := s.db.Exec(`UPDATE sessions SET workspace = ? WHERE workspace = ?`, newWS, oldWS)
	return err
}

// DeleteArchivedOlderThan 删除归档超过 days 天的会话，返回删除条数。
func (s *Store) DeleteArchivedOlderThan(days int) (int64, error) {
	deadline := time.Now().AddDate(0, 0, -days).Unix()
	res, err := s.db.Exec(`DELETE FROM sessions WHERE archived_at > 0 AND archived_at <= ?`, deadline)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListWorkspaces 返回有过会话的全部工作区路径（未归档会话数倒序）。
// 未选工作区（空串）以 "（未选工作区）" 由前端处理，这里返回原始空串键。
func (s *Store) ListWorkspaces() ([]string, error) {
	rows, err := s.db.Query(`SELECT workspace FROM sessions WHERE archived_at = 0 GROUP BY workspace ORDER BY MAX(updated_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, err
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// LatestSession 返回工作区最近更新的会话 id（无会话返回空串）。
func (s *Store) LatestSession(workspace string) (string, error) {
	var id string
	err := s.db.QueryRow(
		`SELECT id FROM sessions WHERE workspace = ? ORDER BY updated_at DESC LIMIT 1`, workspace).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// RenameSession 重命名会话。
func (s *Store) RenameSession(id, title string) error {
	_, err := s.db.Exec(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`,
		title, time.Now().Unix(), id)
	return err
}

// SetSessionWorkspace 更新会话归属的工作区（用于新建的空工作区会话
// 在用户选中工作区后挂靠到该工作区）。
func (s *Store) SetSessionWorkspace(id, workspace string) error {
	_, err := s.db.Exec(`UPDATE sessions SET workspace = ?, updated_at = ? WHERE id = ?`,
		workspace, time.Now().Unix(), id)
	return err
}

// DeleteSession 删除会话及其消息（级联）。
func (s *Store) DeleteSession(id string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return err
}

// DeleteWorkspace 删除整个项目（工作区）下的全部会话及其消息。
func (s *Store) DeleteWorkspace(workspace string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE workspace = ?`, workspace)
	return err
}
