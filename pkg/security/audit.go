package security

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// 审计日志轮转参数。
//
// 之前只有 os.O_APPEND 且无任何大小检查，日志会随运行时间无限增长。
// 现在超过 maxAuditBytes 就轮转，最多保留 maxAuditBackups 份历史
// （.1 最近，.N 最旧）。
//
// 数值取舍：单条 AuditEntry 序列化后约 150~200 字节（action 是完整命令或路径，
// 可能很长）。1 MiB ≈ 5000~7000 条，对单用户本地代理是「几天到几周」的量，
// 足够事后追溯又不至于占满磁盘。
const (
	maxAuditBytes   = 1 << 20 // 单文件上限 1 MiB
	maxAuditBackups = 3       // 保留历史份数
)

// AuditLogger 以 JSONL 形式记录工具调用的安全审计日志。
type AuditLogger struct {
	mu   sync.Mutex
	path string
	f    *os.File
	// size 是当前文件已写字节数。用来避免每条记录都 stat 一次磁盘 ——
	// 审计写在热路径上，stat 的额外 IO 不划算。
	size int64
	// maxBytes / maxBackups 是可注入的阈值，让测试能用小值精确触发轮转，
	// 不必真写 1 MiB 临时文件。生产路径走 NewAuditLogger 的默认值。
	maxBytes   int64
	maxBackups int
}

// NewAuditLogger 打开（或创建）审计日志文件。
func NewAuditLogger(path string) (*AuditLogger, error) {
	return newAuditLogger(path, maxAuditBytes, maxAuditBackups)
}

// newAuditLogger 带可注入阈值的构造函数。
//
// 打开时若文件已超过上限，先轮转一次再打开 —— 否则一个长期没清的历史日志
// 要等到本次会话第一条审计记录时才被动轮转。
func newAuditLogger(path string, maxBytes int64, maxBackups int) (*AuditLogger, error) {
	if path == "" {
		return &AuditLogger{maxBytes: maxBytes, maxBackups: maxBackups}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
		if err := rotate(path, maxBackups); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	return &AuditLogger{path: path, f: f, size: size, maxBytes: maxBytes, maxBackups: maxBackups}, nil
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
	data = append(data, '\n')

	a.mu.Lock()
	defer a.mu.Unlock()

	// 单条就超上限的极端情况（action 是个几百 MB 的字符串）不在这里处理：
	// 正常审计记录不会这么大，真出现了也不该靠丢记录来兜。
	if a.size+int64(len(data)) > a.maxBytes {
		if err := a.rotateLocked(); err != nil {
			// 轮转失败**不静默**：审计日志是安全追溯的凭据，
			// 悄悄丢了比报错更糟。错误上抛，由调用方决定怎么办。
			return err
		}
	}
	n, err := a.f.Write(data)
	a.size += int64(n)
	return err
}

// rotateLocked 执行轮转。调用方必须已持有 a.mu。
func (a *AuditLogger) rotateLocked() error {
	if err := rotate(a.path, a.maxBackups); err != nil {
		return err
	}
	// 原句柄还开着旧 inode，必须换成新文件，否则写进去的仍是轮转前的那个
	if err := a.f.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		a.f = nil
		return err
	}
	a.f = f
	a.size = 0
	return nil
}

// rotate 把 path 移到 path.1，旧的 .1 移到 .2 …… 并删除超出保留份数的最旧档。
//
// .1 表示最近一份。轮转后共有 backups 个档：.1=旧主文件、.2=旧 .1 ……
// 所以要删的是 **.N+1**（那份最旧的），不是 .N —— 删 .N 会让更老的 .N+1
// 原地残留，成为永远清不掉的僵尸档。
//
// 搬移**从后往前**，否则 .1→.2 之后 .2 已被占用，.2→.3 又会覆盖刚搬过去的。
func rotate(path string, backups int) error {
	if backups < 1 {
		backups = 1
	}
	// 先删掉会溢出的最旧档
	if err := os.Remove(archiveName(path, backups+1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	// 从后往前搬：.N → .N+1 会冲突，所以只搬 .1..N-1
	for i := backups - 1; i >= 1; i-- {
		from, to := archiveName(path, i), archiveName(path, i+1)
		if _, err := os.Stat(from); err != nil {
			continue // 不存在就跳过，不要让空档阻断轮转
		}
		if err := os.Rename(from, to); err != nil {
			return err
		}
	}
	return os.Rename(path, archiveName(path, 1))
}

func archiveName(path string, n int) string {
	return path + "." + strconv.Itoa(n)
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
