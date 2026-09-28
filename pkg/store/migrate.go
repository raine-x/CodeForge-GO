package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"codeforge/pkg/llm"
	"codeforge/pkg/logx"
)

// MigrateSessions 导入旧版 JSON 会话文件（.codeforge/<id>.json）。
//
// 旧布局没有 workspace 概念（DataDir 相对路径随工作区漂移），这里统一把
// 迁移来源的目录反推为 workspace：JSON 所在 .codeforge 的上一级目录即工作区。
// 导入成功后原文件改名为 <id>.json.imported 保留（不删除，回滚有据可查）；
// 已存在 .imported 标记或同 id 会话时跳过，保证幂等。
func (s *Store) MigrateSessions(legacyDir string) (imported int) {
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		return 0 // 目录不存在：全新安装，无旧数据
	}
	workspace := inferWorkspace(legacyDir)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(legacyDir, e.Name())
		n, err := importOne(s, full, workspace)
		if err != nil {
			logx.Errorf("迁移会话 %s 失败: %v", e.Name(), err)
			continue
		}
		imported += n
	}
	return imported
}

// importOne 导入单个 JSON 文件；已导入过（.imported 存在或 id 已在库中）返回 0。
func importOne(s *Store, full, workspace string) (int, error) {
	if _, err := os.Stat(full + ".imported"); err == nil {
		return 0, nil // 上次迁移已处理
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return 0, err
	}
	// 旧文件时间字段是 RFC3339 字符串（agent.Session 的序列化格式），
	// 不能直接按 int64 解（Unmarshal 整体报错），逐字段兼容解析。
	var sess struct {
		ID       string        `json:"id"`
		Title    string        `json:"title"`
		Messages []llm.Message `json:"messages"`
	}
	if err := json.Unmarshal(data, &sess); err != nil {
		return 0, err
	}

	row := SessionRow{
		ID:        sess.ID,
		Workspace: workspace,
		Title:     firstNonEmpty(sess.Title, "未命名会话"),
		CreatedAt: timeOrNow(parseTimeField(data, "created_at")),
		UpdatedAt: timeOrNow(parseTimeField(data, "updated_at")),
		Messages:  sess.Messages,
	}
	if row.ID == "" {
		return 0, nil // 空文件（旧版每次启动建的空会话），跳过
	}
	if _, ok, _ := s.GetSession(row.ID); ok {
		_ = os.Rename(full, full+".imported") // 库里已有（可能上次导入一半）：补标记
		return 0, nil
	}
	if err := s.CreateSession(row.ID, row.Workspace, row.Title, row.CreatedAt); err != nil {
		return 0, err
	}
	if err := s.SaveSession(row); err != nil {
		_ = s.DeleteSession(row.ID) // 回滚半成品
		return 0, err
	}
	if err := os.Rename(full, full+".imported"); err != nil {
		logx.Errorf("标记已迁移文件失败（不影响数据）: %v", err)
	}
	return 1, nil
}

// inferWorkspace 由 .codeforge 目录反推工作区根（其父目录）。
func inferWorkspace(legacyDir string) string {
	parent := filepath.Dir(strings.TrimRight(legacyDir, `\/`))
	if parent == "" || parent == "." {
		return ""
	}
	return parent
}

// ---------- 旧格式时间字段兼容：旧文件时间可能是 RFC3339 字符串 ----------

type rawJSON map[string]json.RawMessage

func rawField(data []byte, key string) json.RawMessage {
	var m rawJSON
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	return m[key]
}

// parseTimeField 旧文件时间字段兼容解析：先按字符串（RFC3339），再按数字。
func parseTimeField(data []byte, key string) int64 {
	var s string
	if err := json.Unmarshal(rawField(data, key), &s); err == nil {
		if t, ok := parseRFC3339(s); ok {
			return t
		}
	}
	var n int64
	_ = json.Unmarshal(rawField(data, key), &n)
	return n
}

func parseRFC3339(s string) (int64, bool) {
	t, err := timeParse(s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}
