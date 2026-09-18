package store

import (
	"time"
)

// CheckpointRow 是一条文件写入前的检查点（快照）。
//
// 语义：在「步骤 Step」上，Path 这个文件被改写之前的内容是 OldContent。
//   - Existed=true  → 回滚时把 OldContent 写回该路径；
//   - Existed=false → 该文件当时并不存在，回滚时删除它。
type CheckpointRow struct {
	Step       int       `json:"step"`
	Path       string    `json:"path"`
	Existed    bool      `json:"existed"`
	OldContent string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

// CheckpointStep 是一个「回滚点」的聚合视图：某一步骤共改动了多少文件、最近一次何时。
// 前端 actions 栏的回滚菜单按它列出候选（时间 + 文件数）。
type CheckpointStep struct {
	Step   int       `json:"step"`
	Files  int       `json:"files"`
	At     time.Time `json:"at"`
	Sample string    `json:"sample,omitempty"` // 首个被改动文件的路径（菜单里给个直观提示）
}

// InsertCheckpoint 记录一条检查点。
//
// 按主键 (session_id, step, path) 去重：同一步骤内对同一文件的重复写入
// 只保留**第一次**（即该步骤开始前的内容）——回滚要的是「这一步之前是什么样」，
// 中间态没有意义。用 INSERT OR IGNORE 让数据库承担去重，调用方无需查重。
func (s *Store) InsertCheckpoint(sessionID string, row CheckpointRow) error {
	existed := 0
	if row.Existed {
		existed = 1
	}
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO checkpoints (session_id, step, path, existed, old_content, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, row.Step, row.Path, existed, row.OldContent, time.Now().Unix())
	return err
}

// ListCheckpoints 列出会话的全部检查点（步骤倒序、同步骤内按路径排序）。
func (s *Store) ListCheckpoints(sessionID string) ([]CheckpointRow, error) {
	rows, err := s.db.Query(
		`SELECT step, path, existed, old_content, created_at FROM checkpoints
		 WHERE session_id = ? ORDER BY step DESC, path ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckpointRow
	for rows.Next() {
		var r CheckpointRow
		var existed int
		var created int64
		if err := rows.Scan(&r.Step, &r.Path, &existed, &r.OldContent, &created); err != nil {
			return nil, err
		}
		r.Existed = existed != 0
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListCheckpointSteps 列出会话的回滚点（按步骤聚合，步骤倒序）。
func (s *Store) ListCheckpointSteps(sessionID string) ([]CheckpointStep, error) {
	rows, err := s.db.Query(
		`SELECT step, COUNT(*), MAX(created_at), MIN(path)
		 FROM checkpoints WHERE session_id = ? GROUP BY step ORDER BY step DESC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckpointStep
	for rows.Next() {
		var st CheckpointStep
		var at int64
		if err := rows.Scan(&st.Step, &st.Files, &at, &st.Sample); err != nil {
			return nil, err
		}
		st.At = time.Unix(at, 0)
		out = append(out, st)
	}
	return out, rows.Err()
}

// CheckpointsFrom 列出「步骤 >= fromStep」的检查点（同一路径可能跨多步出现）。
// 调用方按 Step 倒序逐条回滚，才能保证每一步都是回到「该步之前」的状态。
func (s *Store) CheckpointsFrom(sessionID string, fromStep int) ([]CheckpointRow, error) {
	rows, err := s.db.Query(
		`SELECT step, path, existed, old_content, created_at FROM checkpoints
		 WHERE session_id = ? AND step >= ? ORDER BY step DESC, path ASC`,
		sessionID, fromStep)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckpointRow
	for rows.Next() {
		var r CheckpointRow
		var existed int
		var created int64
		if err := rows.Scan(&r.Step, &r.Path, &existed, &r.OldContent, &created); err != nil {
			return nil, err
		}
		r.Existed = existed != 0
		r.CreatedAt = time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteCheckpointsFrom 删除「步骤 >= fromStep」的检查点。
//
// 回滚成功后必须调用：这些检查点描述的是**已被撤销**的写入，
// 留着会让「再回滚一次」把已经还原的文件又写回旧内容。
// 反过来说，回滚到 toStep 会连带丢弃 toStep 及之后的全部检查点，
// 因为任务已经从那里重新开始了（与消息截断一致）。
func (s *Store) DeleteCheckpointsFrom(sessionID string, fromStep int) (int64, error) {
	res, err := s.db.Exec(
		`DELETE FROM checkpoints WHERE session_id = ? AND step >= ?`, sessionID, fromStep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
