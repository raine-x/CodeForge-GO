// spill.go：工具结果外置（context offloading）。
//
// 问题：Executor 对超过 maxOutput 的结果做的是**硬截断**（见 executor.go 的
// truncate）—— 被砍掉的部分**永远消失**，模型手里只剩一段预览和一个
// 「原始长度 N 字节」的提示。做 APK 逆向这类负载时，一个 unzip -l / jadx
// 的输出就吃掉几万 token，十来次调用就把 128k 窗口填满，触发压缩。
//
// 做法：超过阈值时不截断，而是把**完整内容**落到进程级 scratch
// （~/.codeforge/toolout/<会话>/<id>.txt），上下文里只留一段预览 + 一个
// 短 id。模型想看细节时用 tool_result_read(id, start_line) 按行取回。
//
// 为什么这不等于「丢上下文」：内容一个字节都没少，只是从「常驻上下文」
// 变成「按需取用」。这是本项目里唯一能做到真正无损的路径 —— 摘要与机械
// 压缩都是**有损**的，摘要是有损压缩，机械压缩是直接丢弃。
//
// 落盘位置刻意不进 pkg/backend（同 AGENTS.md §5 决策 2 对撤销副本 scratch 的
// 理由）：它进程级、跨工作区，与「工作区里有哪些文件」无关。进 Backend 等于
// 给远程后端定义「往我本地 home 目录写文件」的契约。
package tools

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// toolOut 相关默认值。
const (
	// DefaultToolOutSpillBytes 是「多大就外置」的默认阈值。
	//
	// 取 8 KiB：按代码/JSON 约 3–3.5 字符/token 的口径，一次外置大约能省下
	// 2000+ token，而预览只占 2 KiB。阈值再大（比如贴着 32 KiB 的执行器上限）
	// 就没意义了 —— 那时窗口早就被填满了。
	DefaultToolOutSpillBytes = 8 * 1024

	// defaultToolOutPreviewBytes 是外置后留在上下文里的预览长度。
	//
	// 只留开头：工具输出的信息密度通常在开头（首条命中、首个错误、头几行
	// 结构）。真需要后面的内容模型会自己去读。
	defaultToolOutPreviewBytes = 2 * 1024

	// toolOutReadMaxBytes 是 tool_result_read 单次返回的上限。
	//
	// 上限必须存在：这个工具的产物本身也要进上下文，一个 5 MB 的文件被它
	// 原样吐回来就等于没外置。按 read_file 的 maxReadBytes 口径对齐。
	toolOutReadMaxBytes = 24 * 1024

	// toolOutMaxIDLen 是外置 id 的长度上限（hex 字符）。
	toolOutMaxIDLen = 16
)

// ToolOutRef 是一次外置的句柄。
type ToolOutRef struct {
	ID    string // 短 id，模型用它回取（tr_ 前缀 + 十六进制）
	Bytes int    // 完整内容的字节数
	Lines int    // 完整内容的行数
}

// ToolOutStore 管理外置文件。
//
// 并发安全：一次对话里多个工具可能并发返回大结果。
type ToolOutStore struct {
	root         string // 根目录；空串表示整个功能关闭
	spillBytes   int
	previewBytes int

	mu sync.Mutex
}

// NewToolOutStore 构造外置存储。root 为空或 spillBytes <= 0 时功能关闭，
// Store 一律返回 spilled=false（调用方随即走原有的截断路径）。
func NewToolOutStore(root string, spillBytes, previewBytes int) *ToolOutStore {
	if root == "" || spillBytes <= 0 {
		return nil
	}
	if previewBytes <= 0 {
		previewBytes = defaultToolOutPreviewBytes
	}
	if previewBytes >= spillBytes {
		previewBytes = spillBytes / 2
	}
	return &ToolOutStore{root: root, spillBytes: spillBytes, previewBytes: previewBytes}
}

// DefaultToolOutRoot 返回默认的外置根目录（~/.codeforge/toolout）。
//
// 与 builtin.undoSpillHome 同源：进程级、跨工作区，且不落在用户的项目里。
func DefaultToolOutRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codeforge", "toolout")
}

// Enabled 报告外置功能是否开启。
func (s *ToolOutStore) Enabled() bool { return s != nil && s.root != "" }

// sessionDir 返回某会话的外置目录。会话 id 为空时用 "_"，
// 对应「拿不到会话域」的调用（如子智能体临时循环）——它们仍能外置，
// 只是不保证跨调用可回取。
func (s *ToolOutStore) sessionDir(sessionID string) string {
	id := sanitizeToolOutSegment(sessionID)
	if id == "" {
		id = "_"
	}
	return filepath.Join(s.root, id)
}

// sanitizeToolOutSegment 把外部字符串收敛成「单个安全路径段」。
//
// 会话 id 来自内部，但这里仍做白名单过滤：拼路径的字符必须完全由本文件
// 控制，否则一处上游改动就能把写入点挪到 root 之外。
func sanitizeToolOutSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// newToolOutID 生成短随机 id。
func newToolOutID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 失败是极端情况；时间戳兜底仍保证同一进程内不重复。
		return fmt.Sprintf("tr_%x", time.Now().UnixNano())
	}
	return "tr_" + hex.EncodeToString(buf[:])
}

// ShouldSpill 报告该内容是否值得外置。
func (s *ToolOutStore) ShouldSpill(text string) bool {
	return s.Enabled() && len(text) > s.spillBytes
}

// Store 把完整内容落盘，返回句柄与留���上下文的预览文本。
//
// 落盘失败不视为错误：返回 spilled=false，调用方随即退回原有的截断行为 ——
// 外置是优化，不该因为它把工具结果整个搞挂。
func (s *ToolOutStore) Store(sessionID, text string) (ToolOutRef, string, bool) {
	if !s.ShouldSpill(text) {
		return ToolOutRef{}, "", false
	}

	id := newToolOutID()
	dir := s.sessionDir(sessionID)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ToolOutRef{}, "", false
	}
	path := filepath.Join(dir, id+".txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		return ToolOutRef{}, "", false
	}

	ref := ToolOutRef{ID: id, Bytes: len(text), Lines: countLines(text)}
	return ref, s.preview(text, id), true
}

// preview 截出留给上下文的那一段，并在尾部挂上可执行的提示。
//
// 提示必须写清「不要凭预览猜测」—— 这是实测里最要紧的一句：
// 模型看到截断的输出，最典型的行为是**自己编一个合理的结尾**，
// 然后基于编出来的内容继续往下写。
func (s *ToolOutStore) preview(text, id string) string {
	head := truncateString(text, s.previewBytes)
	return head + fmt.Sprintf(
		"\n\n"+SpillMarker+"以上是前 %d 字节，共 %d 行 / %d 字节。\n"+
			"完整内容未丢失，需要后续细节时调用 tool_result_read(id=%q, start_line=N) 分段读取。\n"+
			"不要凭这段预览猜测被省略的部分。",
		s.previewBytes, countLines(text), len(text), id)
}

// countLines 按行计数（末尾无换行也算一行）。
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// Read 按行取回外置内容。
//
// sessionID 与 id 都由内部给出并在此做白名单校验，因此不存在路径穿越：
// 拼出来的路径恒在 s.root/<会话>/<id>.txt 之下。
func (s *ToolOutStore) Read(sessionID, id string, startLine, endLine int) (string, error) {
	if !s.Enabled() {
		return "", fmt.Errorf("工具结果外置未启用")
	}
	if err := validateToolOutID(id); err != nil {
		return "", err
	}
	// 会话 id 来自 ctx，模型无法指定；仍做一次收敛，防止上游传入被污染的值。
	sid := sanitizeToolOutSegment(sessionID)
	if sid == "" {
		return "", fmt.Errorf("当前不在会话内，无法读取外置输出")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(filepath.Join(s.root, sid, id+".txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("没有这个外置输出: %s（可能所属会话已删除，或该次结果本来就没外置）", id)
		}
		return "", err
	}

	lines := strings.Split(string(raw), "\n")
	total := len(lines)
	start := startLine
	if start <= 0 {
		start = 1
	}
	if start > total {
		return "", fmt.Errorf("起始行 %d 超出范围（共 %d 行）", start, total)
	}
	end := endLine
	if end <= 0 || end > total {
		end = total
	}
	if end < start {
		return "", fmt.Errorf("end_line(%d) 小于 start_line(%d)", end, start)
	}

	out := strings.Join(lines[start-1:end], "\n")
	if len(out) > toolOutReadMaxBytes {
		// 同 read_file 的口径：截断必须**显式**告知行号范围，否则模型以为读全了。
		kept := truncateString(out, toolOutReadMaxBytes)
		n := strings.Count(kept, "\n") + 1
		out = kept + fmt.Sprintf(
			"\n... [本次读取已达上限 %d 字节，已截断；原始共 %d 行，本次给出第 %d–%d 行]",
			toolOutReadMaxBytes, total, start, start+n-1)
	}
	return fmt.Sprintf("（第 %d–%d 行 / 共 %d 行）\n%s", start, end, total, out), nil
}

// validateToolOutID 只接受本文件生成的 id 形态。
func validateToolOutID(id string) error {
	if id == "" {
		return fmt.Errorf("id 不能为空")
	}
	if len(id) > toolOutMaxIDLen+len("tr_") {
		return fmt.Errorf("id 过长")
	}
	if !strings.HasPrefix(id, "tr_") {
		return fmt.Errorf("id 格式不对")
	}
	for _, r := range id[len("tr_"):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return fmt.Errorf("id 格式不对")
		}
	}
	if len(id) == len("tr_") {
		return fmt.Errorf("id 格式不对")
	}
	return nil
}

// RemoveSession 删掉某会话的全部外置文件。
func (s *ToolOutStore) RemoveSession(sessionID string) error {
	if !s.Enabled() {
		return nil
	}
	sid := sanitizeToolOutSegment(sessionID)
	if sid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(filepath.Join(s.root, sid))
}

// GC 清空整个外置目录。
//
// 与 GCUndoSpill 同一个前提：外置产物是**本次进程会话的缓存**，不是用户
// 的资产 —— 删掉只会让模型重新去跑一次工具。所以可以在启动时整目录清。
func (s *ToolOutStore) GC() error {
	if !s.Enabled() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.root)
}

// SpillMarker 是预览尾部给模型的那段提示（导出让测试能断言其存在）。
const SpillMarker = "【输出已外置】"
