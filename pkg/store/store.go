// Package store 实现基于 SQLite 的持久化存储：会话、消息、用户记忆。
//
// 设计要点：
//   - 库文件为用户级（%USERPROFILE%/.codeforge/data.db），跨工作区共享；
//     会话/记忆用 workspace 字段隔离，切换工作区即切换各自的数据视图。
//   - 驱动用 modernc.org/sqlite（纯 Go 零 CGO），与项目 wazero 的零 CGO 偏好一致。
//   - 消息内容存 llm.ContentBlock 数组的 JSON，与内存结构完全一致，读写零转换。
//   - 单写多读：Store 自身串行化写入（调用方在事务内完成），读走数据库并发安全。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // 注册 sqlite 驱动
)

// Store 是 SQLite 持久化存储。
type Store struct {
	db   *sql.DB
	path string
}

// DefaultPath 返回用户级库文件路径（%USERPROFILE%/.codeforge/data.db）。
// 用户目录不可用时回退当前目录。
func DefaultPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".codeforge", "data.db")
}

// Open 打开（必要时创建）库并建表。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	// busy_timeout：并发读时写锁短暂等待而不是立刻报错
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 单文件库并发写意义有限，限制连接数避免写锁竞争
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Path 返回库文件路径（供测试与诊断）。
func (s *Store) Path() string { return s.path }

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// migrate 建表（幂等）。
func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id          TEXT PRIMARY KEY,
			workspace   TEXT NOT NULL,
			title       TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL,
			archived_at INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_ws ON sessions(workspace, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS messages (
			session_id TEXT NOT NULL,
			seq        INTEGER NOT NULL,
			role       TEXT NOT NULL,
			content    TEXT NOT NULL,
			PRIMARY KEY (session_id, seq),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS memories (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace  TEXT NOT NULL,
			content    TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memories_ws ON memories(workspace)`,
		// 项目显示名：键 = sessions.workspace，值 = 用户起的显示名。
		// 与 workspace 键解耦 —— 重命名项目**只动这张表**，绝不改写会话归属键，
		// 否则会把工作区路径抹成一个裸名字（文件工具随即失效）。
		`CREATE TABLE IF NOT EXISTS workspace_names (
			workspace  TEXT PRIMARY KEY,
			name       TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// 升级路径：旧库补归档列（CREATE TABLE IF NOT EXISTS 不会给已存在的表加列）
		`ALTER TABLE sessions ADD COLUMN archived_at INTEGER NOT NULL DEFAULT 0`,
		// 任务清单：会话级 todo 表（todo_write 整体替换语义，见 sessions.go）
		`CREATE TABLE IF NOT EXISTS session_todos (
			session_id  TEXT NOT NULL,
			sort        INTEGER NOT NULL,
			content     TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'pending',
			priority    INTEGER NOT NULL DEFAULT 0,
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (session_id, sort),
			FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_todos_session ON session_todos(session_id, sort)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			// ALTER 失败且原因是列已存在 → 正常（旧库升级过的重复执行）
			if strings.Contains(err.Error(), "duplicate column") {
				continue
			}
			return fmt.Errorf("初始化表结构失败: %w", err)
		}
	}
	return nil
}
