// Package backend 定义「纯 IO 执行环境」的最小契约。
//
// 分层纪律（AGENTS.md §5）：
//   - Backend 只做 IO，不含任何「是否在工作区内」的判断；
//   - 会话级守卫（越界围栏、指纹、阅读登记、撤销栈）在 builtin.Guard；
//   - 工具构造函数只拿 Guard，Guard 内部持有 Backend。
//
// 实现者必须遵守的不变式（契约测试逐条钉住）：
//
//  1. Realpath 必须复刻 builtin 包的逐级向上语义：自 path 起逐级找第一个
//     能被完整解析的祖先，把**尚未存在的尾部组件**拼回。
//     不可简化成「只对最终组件求解析」—— 新建文件的解析必然失败，
//     而「区内软链指向区外、目标文件尚不存在」正是靠这一步挡住。
//  2. Stat 是 stat 语义（跟随软链）；List 返回的 Entry 是 lstat 语义
//     （不跟随）。后者不是 bug，是既有行为，抽象层不许「顺手修正」。
//  3. 目录遍历的失败语义**分两档**：递归吞错（根不存在返回空列表 + nil），
//     非递归 fail-fast（根读不了报错）。这是既有行为，不是疏漏。
//  4. WriteFile 必须是**原子**覆盖写（同目录临时文件 → 写 → rename）。
//     「打开→截断→写」在进程写一半被杀时留半截文件。
package backend

import (
	"context"
	"errors"
	"os"
	"time"
)

// ErrNotSupported 表示该后端不支持此能力。
//
// 它不是「降级」信号。契约测试里凡是能故意返回它的方法，都是在证明
// **调用方没有隐式依赖该能力** —— 即「换一个没有本地 shell 的后端时，
// run_command 仍有一条干净的失败路径」，而不是 panic 或静默返回空输出。
var ErrNotSupported = errors.New("backend: 该后端不支持此能力")

// DefaultSkipDirs 是所有后端共享的默认跳过目录名集合。
var DefaultSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "vendor": true,
	".idea": true, ".vscode": true, "__pycache__": true, ".codeforge": true,
}

// Info 是一个文件或目录的元信息。
//
// ⚠️ 语义由产生它的方法决定，不可混用：
//   - Stat 返回 stat 语义（跟随软链）：软链→目录 得 IsDir=true；
//   - List 返回 lstat 语义（不跟随）：软链→目录 得 IsDir=false。
type Info struct {
	Name  string
	IsDir bool
	Size  int64
}

// Entry 是 List 的一项。Path 是绝对路径，分隔符随后端。
type Entry struct {
	Path string
	Info Info
}

// ListOptions 是 List 的选项。
type ListOptions struct {
	// Recursive 为真时递归整个子树，产出文件与目录两类条目。
	//
	// 两条分支的失败语义**不同**，契约分别钉住：
	//   - true  ：逐条吞错，连 walkErr 都丢弃 ⇒ 对不存在的目录返回
	//     空列表 + nil error；
	//   - false ：根读不了直接报错，但单条 Info() 失败仍被跳过。
	Recursive bool
	// SkipDirs 为 nil 时使用 DefaultSkipDirs 的副本。
	// 传非 nil 的空 map 表示「一个都不跳过」。
	SkipDirs map[string]bool
}

// GrepOptions 是内容检索的选项。
type GrepOptions struct {
	// Pattern 是**纯子串**，不是正则 —— 换成正则是一次独立的语义变更。
	Pattern string
	// Glob 只对**基名**做匹配，不跨目录分隔符。匹配错误被吞掉等同不匹配。
	Glob string
	// CaseSensitive 为假时两侧都折叠大小写。
	CaseSensitive bool
	// MaxFileSize 是单文件扫描上限。<=0 表示「无上限」——
	// 生产路径总是显式给上限。
	MaxFileSize int64
	// MaxResults <=0 表示无上限。
	MaxResults int
	// SkipDirs 同 ListOptions。
	SkipDirs map[string]bool
}

// Match 是一条命中。
type Match struct {
	Path string
	Line int // 1-based
	Text string
}

// GrepResult 是内容检索的结果。
type GrepResult struct {
	Matches []Match
	// Scanned 是**真正打开并扫描过**的文件数。
	Scanned int
	// Truncated 为真表示因达到 MaxResults 提前停止。
	// ⚠️ 截断时必须返回 nil error，不能把「提前中止」冒泡成失败。
	Truncated bool
}

// GlobOptions 是按名查找的选项。
type GlobOptions struct {
	Pattern    string
	MaxResults int
	SkipDirs   map[string]bool
}

// GlobResult 是按名查找的结果。
type GlobResult struct {
	// Paths 是**相对 Root** 的路径，分隔符随后端。
	Paths []string
	// Scanned 是**访问到的所有非目录条目数**（不分是否命中）。
	//
	// ⚠️ 与 GrepResult.Scanned 不是同一口径 —— 一个数「扫描过」，
	// 一个数「路过」。契约必须分别断言，否则有人会顺手统一，
	// 而统一的���果是 search_files 的 meta 数字变了。
	Scanned   int
	Truncated bool
}

// ExecRequest 是一次命令执行请求。
type ExecRequest struct {
	// Command 是交给平台默认 shell 解释的**一整条命令行**。后端不解释它。
	Command string
	// Dir 是工作目录，已由 Guard 完成围栏校验。
	Dir string
	// Timeout <=0 表示后端自行取默认值。
	Timeout time.Duration
}

// ExecResult 是一次命令执行的结果。
type ExecResult struct {
	// Output 是 stdout 与 stderr **合并**后的内容。
	Output string
	// ExitCode 沿用既有语义：0 成功；-2 超时被终止；-1 其它启动/等待失败。
	ExitCode int
	Duration time.Duration
	// Shell 是实际使用的解释器名。
	Shell string
	// TimedOut 为真时 ExitCode 必为 -2。
	TimedOut bool
	// StartErr 非 nil 表示进程根本没起来。此时其余字段无意义。
	StartErr error
}

// Backend 是执行环境的纯 IO 契约。
//
// 10 个方法，没有一个知道「工作区」是什么。
type Backend interface {
	// ReadFile 读整个文件。目标不存在或不是普通文件时返回错误。
	ReadFile(path string) ([]byte, error)

	// WriteFile **原子**覆盖写。失败路径必须清掉临时文件。
	WriteFile(path string, data []byte, perm os.FileMode) error

	// Stat 是 stat 语义（**跟随**软链）。
	Stat(path string) (Info, error)

	// List 列目录，见 ListOptions。
	List(dir string, opts ListOptions) ([]Entry, error)

	// Remove 删单个文件。目录仅在为空时可删。
	Remove(path string) error

	// MkdirAll 建目录及全部父级，幂等。
	MkdirAll(path string, perm os.FileMode) error

	// Realpath 解析软链，返回真实路径。不可简化为「只解析末段」。
	Realpath(path string) (string, error)

	// Grep 按内容检索。
	Grep(ctx context.Context, root string, opts GrepOptions) (GrepResult, error)

	// Glob 按文件名模式查找。
	Glob(ctx context.Context, root, pattern string, opts GlobOptions) (GlobResult, error)

	// Exec 执行一条 shell 命令。
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)
}
