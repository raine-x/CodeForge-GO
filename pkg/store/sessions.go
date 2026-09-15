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
// WorkspaceName 是所属项目的**显示名**（来自 workspace_names 表，未设置时为空串，
// 由前端回落到路径末段），与 Workspace（真实工作区键）解耦。
type SessionMetaRow struct {
	ID            string    `json:"id"`
	Workspace     string    `json:"workspace"`
	WorkspaceName string    `json:"workspace_name"`
	Title         string    `json:"title"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	MessageCount  int       `json:"message_count"`
	ArchivedAt    int64     `json:"archived_at"` // 0 = 未归档；>0 = 归档时间戳（unix 秒）
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
// 顺带 LEFT JOIN workspace_names 带上项目显示名（没有自定义名时为 ''）。
func (s *Store) ListSessions(workspace string, archived bool) ([]SessionMetaRow, error) {
	q := `SELECT s.id, s.workspace, COALESCE(n.name, ''), s.title, s.created_at, s.updated_at,
	      s.archived_at, (SELECT COUNT(*) FROM messages m WHERE m.session_id = s.id) AS cnt
	      FROM sessions s LEFT JOIN workspace_names n ON n.workspace = s.workspace
	      WHERE s.archived_at ` + opArchived(archived)
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
		if err := rows.Scan(&m.ID, &m.Workspace, &m.WorkspaceName, &m.Title,
			&created, &updated, &m.ArchivedAt, &m.MessageCount); err != nil {
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

// UnarchiveWorkspace 把整个工作区的已归档会话一次性恢复。
// 与 ArchiveWorkspace 对称：侧栏的「归档」是一次点掉整组，恢复不该让用户逐条点 N 次。
func (s *Store) UnarchiveWorkspace(workspace string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET archived_at = 0 WHERE workspace = ? AND archived_at > 0`,
		workspace)
	return err
}

// SetWorkspaceName 设置项目的**显示名**（存 workspace_names 表）。
//
// ⚠️ 这里刻意不改 sessions.workspace：那个键同时是 agent 的工作目录路径，
// 一改就会让「重命名项目」变成「把工作区路径抹成一个裸名字」，之后文件工具全部失效
// （历史上就是这么把 C:\...\Desktop\test 变成 test、把中文名变成 ???? 的）。
// name 传空串 = 清除自定义名，前端回落到按路径末段显示。
func (s *Store) SetWorkspaceName(workspace, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		_, err := s.db.Exec(`DELETE FROM workspace_names WHERE workspace = ?`, workspace)
		return err
	}
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO workspace_names (workspace, name, updated_at) VALUES (?, ?, ?)`,
		workspace, name, time.Now().Unix())
	return err
}

// WorkspaceName 返回某个项目的显示名（未设置返回空串）。
func (s *Store) WorkspaceName(workspace string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM workspace_names WHERE workspace = ?`, workspace).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// DeleteWorkspaceName 删除某个项目的显示名记录（项目被删时一并清理）。
func (s *Store) DeleteWorkspaceName(workspace string) error {
	_, err := s.db.Exec(`DELETE FROM workspace_names WHERE workspace = ?`, workspace)
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

// DeleteWorkspace 删除整个项目（工作区）下的全部会话及其消息，并清掉它的显示名记录。
func (s *Store) DeleteWorkspace(workspace string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE workspace = ?`, workspace); err != nil {
		return err
	}
	return s.DeleteWorkspaceName(workspace)
}

// TodoRow 是会话任务清单的一条记录。
type TodoRow struct {
	Content  string `json:"content"`  // 任务内容
	Status   string `json:"status"`   // pending / in_progress / completed / cancelled
	Priority int    `json:"priority"` // 0=普通 1=优先
	Sort     int    `json:"sort"`     // 排序序号（行内索引）
}

// ListTodos 列出会话的全部任务（按 sort 升序）。
func (s *Store) ListTodos(sessionID string) ([]TodoRow, error) {
	rows, err := s.db.Query(
		`SELECT content, status, priority, sort FROM session_todos WHERE session_id = ? ORDER BY sort ASC`,
		sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TodoRow
	for rows.Next() {
		var r TodoRow
		if err := rows.Scan(&r.Content, &r.Status, &r.Priority, &r.Sort); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplaceTodos 用整表数组整体替换会话的任务清单（todo_write 的
// 「Claude TodoWrite 语义」：模型每次提交完整列表，缺项即删除）。
// 在事务里先清后插，保证前后一致。
func (s *Store) ReplaceTodos(sessionID string, todos []TodoRow) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM session_todos WHERE session_id = ?`, sessionID); err != nil {
		return err
	}
	now := time.Now().Unix()
	for i, t := range todos {
		if _, err := tx.Exec(
			`INSERT INTO session_todos (session_id, sort, content, status, priority, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, i, t.Content, t.Status, t.Priority, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
