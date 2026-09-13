package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry 是一条审计记录。
type AuditEntry struct {
	Time       string `json:"time"`
	Tool       string `json:"tool"`
	Action     string `json:"action"`
	Decision   string `json:"decision"`
	Approved   *bool  `json:"approved,omitempty"`
	Success    bool   `json:"success"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

// AuditLogger 以 JSONL 形式记录工具调用的安全审计日志。
type AuditLogger struct {
	mu   sync.Mutex
	path string
	f    *os.File
}

// NewAuditLogger 打开（或创建）审计日志文件。
func NewAuditLogger(path string) (*AuditLogger, error) {
	if path == "" {
		return &AuditLogger{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &AuditLogger{path: path, f: f}, nil
}

// Log 写入一条审计记录。
func (a *AuditLogger) Log(e AuditEntry) error {
	if a == nil || a.f == nil {
		return nil
	}
	if e.Time == "" {
		e.Time = time.Now().Format(time.RFC3339)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, err = a.f.Write(append(data, '\n'))
	return err
}

// Path 返回审计日志路径。
func (a *AuditLogger) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// Close 关闭审计日志文件。
func (a *AuditLogger) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.f.Close()
	a.f = nil
	return err
}
