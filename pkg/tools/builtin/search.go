package builtin

import (
	"context"
	"encoding/json"
	"strings"

	"codeforge/pkg/backend"
	"codeforge/pkg/errs"
	"codeforge/pkg/tools"
)

const (
	searchMaxFileSize = 2 << 20 // 单文件最大扫描 2MB
	searchDefaultMax  = 200
)

// SearchTool 在目录中按关键字检索文件内容（纯 Go 实现，不依赖 ripgrep）。
type SearchTool struct{ fs *FS }

// NewSearchTool 构造 search_files 工具。
func NewSearchTool(fs *FS) *SearchTool { return &SearchTool{fs: fs} }

// Name 实现 tools.Tool。
func (t *SearchTool) Name() string { return "search_files" }

// Description 实现 tools.Tool。
func (t *SearchTool) Description() string {
	return "在指定目录下按关键字检索文件**内容**（grep 语义，返回 文件:行号:内容），不是按文件名查找。默认跳过 .git / node_modules / dist / vendor 等目录。按文件名找文件请用 find_files。"
}

// InputSchema 实现 tools.Tool。
func (t *SearchTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("query", "要检索的关键字", true).
		Str("path", "检索起始目录（必须位于工作区内），留空使用工作区根目录", false).
		Str("glob", "文件名过滤，如 *.go（可选）", false).
		Bool("case_sensitive", "是否区分大小写（默认 false）", false).
		Int("max_results", "最大返回条数，默认 200", false).
		Build()
}

// IsReadOnly 声明只读。
func (t *SearchTool) IsReadOnly() bool { return true }

// Metadata 声明副作用等级。
func (t *SearchTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}

// OutsideScope 实现 tools.ScopeChecker。
func (t *SearchTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

type matchItem struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Execute 实现 tools.Tool。
func (t *SearchTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Query         string `json:"query"`
		Path          string `json:"path"`
		Glob          string `json:"glob"`
		CaseSensitive bool   `json:"case_sensitive"`
		MaxResults    int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if strings.TrimSpace(p.Query) == "" {
		return tools.Err("query 不能为空"), nil
	}
	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = searchDefaultMax
	}

	root, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("搜索内容", err)), nil
	}
	needle := p.Query
	if !p.CaseSensitive {
		needle = strings.ToLower(needle)
	}

	matches := make([]matchItem, 0, maxResults)
	scanned := 0

	// 检索委托给 Backend：远程后端一次远端命令即可完成，
	// 留在工具层就等于默认「本地遍历」实现 —— 远程会退化成几千次往返。
	//
	// 参数解析、结果整形、meta 仍在工具层；遍历与匹配在后端。
	res, err := t.fs.backend().Grep(ctx, root, backend.GrepOptions{
		Pattern:       p.Query,
		Glob:          p.Glob,
		CaseSensitive: p.CaseSensitive,
		MaxResults:    maxResults,
		MaxFileSize:   searchMaxFileSize,
	})
	if err != nil {
		return tools.Err("检索失败: %v", err), nil
	}
	for _, m := range res.Matches {
		matches = append(matches, matchItem{File: m.Path, Line: m.Line, Text: m.Text})
	}
	scanned = res.Scanned

	return tools.OkMeta(matches, map[string]any{
		"root":    root,
		"query":   p.Query,
		"count":   len(matches),
		"scanned": scanned,
	}), nil
}

// FindFilesTool 按文件名模式查找文件（非内容检索）。
type FindFilesTool struct{ fs *FS }

func NewFindFilesTool(fs *FS) *FindFilesTool { return &FindFilesTool{fs: fs} }

func (t *FindFilesTool) Name() string { return "find_files" }

func (t *FindFilesTool) Description() string {
	return "在指定目录下按文件名模式查找文件（如 *.py、8.py、test_*.go），返回相对路径列表。默认跳过 .git / node_modules / dist / vendor 等目录。内容检索请用 search_files。"
}

func (t *FindFilesTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("pattern", "文件名模式（glob，如 *.py、8.py、test_*.go）", true).
		Str("path", "检索起始目录（必须位于工作区内），留空使用工作区根目录", false).
		Int("max_results", "最大返回条数，默认 200", false).
		Build()
}

func (t *FindFilesTool) IsReadOnly() bool { return true }

// Metadata 声明副作用等级。
func (t *FindFilesTool) Metadata() tools.Metadata {
	return tools.Metadata{SideEffect: tools.SideEffectNone}
}

func (t *FindFilesTool) OutsideScope(args json.RawMessage) bool { return t.fs.OutsideScopePath(args) }

type findItem struct {
	Path string `json:"path"`
}

func (t *FindFilesTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if r := t.fs.noWorkspace(); r != nil {
		return r, nil
	}
	var p struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return tools.Err("pattern 不能为空"), nil
	}
	maxResults := p.MaxResults
	if maxResults <= 0 {
		maxResults = searchDefaultMax
	}

	root, err := t.fs.ResolveCheckedCtx(ctx, p.Path)
	if err != nil {
		return tools.Err("%s", errs.FriendlyOr("查找文件", err)), nil
	}

	matches := make([]findItem, 0, maxResults)
	scanned := 0

	// 同上：按名查找也交给 Backend，路径在后端侧就算成相对路径。
	res, err := t.fs.backend().Glob(ctx, root, p.Pattern, backend.GlobOptions{
		MaxResults: maxResults,
	})
	if err != nil {
		return tools.Err("查找失败: %v", err), nil
	}
	for _, p2 := range res.Paths {
		matches = append(matches, findItem{Path: p2})
	}
	scanned = res.Scanned

	return tools.OkMeta(matches, map[string]any{
		"root":    root,
		"pattern": p.Pattern,
		"count":   len(matches),
		"scanned": scanned,
	}), nil
}

// RegisterSearch 将检索工具注册到注册中心。
func RegisterSearch(reg *tools.Registry, fs *FS) {
	reg.Register(NewSearchTool(fs))
	reg.Register(NewFindFilesTool(fs))
}
