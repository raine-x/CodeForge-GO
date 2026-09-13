package store

import (
	"time"
)

// MemoryRow 是一条用户记忆。
type MemoryRow struct {
	ID        int64     `json:"id"`
	Workspace string    `json:"workspace"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AddMemory 新增一条记忆，返回自增 id。
func (s *Store) AddMemory(workspace, content string) (int64, error) {
	now := time.Now().Unix()
	res, err := s.db.Exec(
		`INSERT INTO memories (workspace, content, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		workspace, content, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListMemories 列出工作区全部记忆（新→旧）。workspace 为空返回全部。
func (s *Store) ListMemories(workspace string) ([]MemoryRow, error) {
	q := `SELECT id, workspace, content, created_at, updated_at FROM memories`
	var args []any
	if workspace != "" {
		q += ` WHERE workspace = ?`
		args = append(args, workspace)
	}
	q += ` ORDER BY id DESC`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MemoryRow
	for rows.Next() {
		var m MemoryRow
		var created, updated int64
		if err := rows.Scan(&m.ID, &m.Workspace, &m.Content, &created, &updated); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(created, 0)
		m.UpdatedAt = time.Unix(updated, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// UpdateMemory 更新记忆内容。
func (s *Store) UpdateMemory(id int64, content string) error {
	_, err := s.db.Exec(`UPDATE memories SET content = ?, updated_at = ? WHERE id = ?`,
		content, time.Now().Unix(), id)
	return err
}

// DeleteMemory 删除一条记忆。
func (s *Store) DeleteMemory(id int64) error {
	_, err := s.db.Exec(`DELETE FROM memories WHERE id = ?`, id)
	return err
}
