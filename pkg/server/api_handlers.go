package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/errs"
	"codeforge/pkg/llm"
	"codeforge/pkg/platform"
)

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr 是**所有面向用户的失败响应**的统一出口。
//
// 为什么要有它：此前各 handler 直接写 `"error": err.Error()`，
// 于是 "unexpected EOF"、"Access is denied."、SQLite 的原始报错
// 都会原样显示到界面上 —— 用户既看不出发生了什么，也不知道该怎么办。
//
// 这里统一走 errs.FriendlyOr：
//   - 认得出的系统级故障 → 补上中文成因与处置建议；
//   - 已经是人话的业务错误（如「子智能体越权：…」）→ 原样返回，不套废话。
//
// action 用动宾短语，如「创建会话」「保存模型库」。
func writeErr(w http.ResponseWriter, status int, action string, err error) {
	writeJSON(w, status, map[string]any{"error": errs.FriendlyOr(action, err)})
}

// handlePerm 查询 / 热切换权限模式（readonly/ask/auto）。
//
// GET  返回服务端当前生效的模式（前端每次展开弹层都实时拉取，不信本地缓存）；
// POST 切换模式：改策略引擎 + 持久化到 local.yaml。
func (s *Server) handlePerm(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"mode": s.executor.Policy().Mode()})

	case http.MethodPost:
		var body struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		mode := strings.ToLower(strings.TrimSpace(body.Mode))
		if mode != "readonly" && mode != "ask" && mode != "auto" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效权限模式: " + body.Mode})
			return
		}
		s.executor.Policy().SetMode(mode)
		s.cfg.Security.PermissionMode = mode
		_ = s.cfg.SaveState()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": mode})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleHealth 健康检查（免鉴权）。
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"platform": platformName(),
		"version":  "1.0.0",
		"tools":    len(s.registry.Names()),
	})
}

// handleConfig 读取 / 更新 LLM 配置。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.configView())

	case http.MethodPost:
		var body struct {
			Provider    string          `json:"provider"`
			BaseURL     *string         `json:"base_url"`
			APIKey      string          `json:"api_key"`
			Model       string          `json:"model"`
			MaxTokens   int             `json:"max_tokens"`
			Temperature *float64        `json:"temperature"`
			MaxSteps    json.RawMessage `json:"max_steps"`
			// 重试策略：<=0 / 空值表示「不改」（保持原配置）。
			RetryMaxAttempts *int    `json:"retry_max_attempts"`
			RetryMode        *string `json:"retry_mode"`
			RetryIntervalSec *int    `json:"retry_interval_sec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败: " + err.Error()})
			return
		}

		next := *s.cfg
		if body.MaxSteps != nil {
			var n int
			if err := json.Unmarshal(body.MaxSteps, &n); err != nil || n < 1 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "max_steps 必须是正整数"})
				return
			}
			next.Agent.MaxSteps = n
		}

		if body.RetryMode != nil {
			mode := strings.ToLower(strings.TrimSpace(*body.RetryMode))
			if mode != "fixed" && mode != "backoff" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "无效的重试间隔模式: " + *body.RetryMode})
				return
			}
			next.LLM.RetryMode = mode
		}
		if body.RetryMaxAttempts != nil {
			n := *body.RetryMaxAttempts
			if n < 1 || n > 15 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "重试次数需在 1~15 之间"})
				return
			}
			next.LLM.MaxAttempts = n
		}
		if body.RetryIntervalSec != nil {
			n := *body.RetryIntervalSec
			if n < 1 || n > 60 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "重试间隔需在 1~60 秒之间"})
				return
			}
			next.LLM.RetryBackoffMs = n * 1000
		}

		if strings.TrimSpace(body.Provider) != "" {
			next.LLM.Provider = strings.TrimSpace(body.Provider)
		}
		if body.BaseURL != nil {
			next.LLM.BaseURL = strings.TrimSpace(*body.BaseURL)
		}
		if strings.TrimSpace(body.APIKey) != "" {
			next.LLM.APIKey = strings.TrimSpace(body.APIKey)
		}
		if strings.TrimSpace(body.Model) != "" {
			next.LLM.Model = strings.TrimSpace(body.Model)
		}
		if body.MaxTokens > 0 {
			next.LLM.MaxTokens = body.MaxTokens
		}
		if body.Temperature != nil {
			next.LLM.Temperature = *body.Temperature
		}

		var provider llm.Provider
		if next.LLM != s.cfg.LLM {
			var err error
			provider, err = llm.NewProvider(next.LLM)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "创建模型提供方", err)
				return
			}
		}
		if err := next.SaveState(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存配置失败: " + err.Error()})
			return
		}
		s.cfg.LLM = next.LLM
		if body.MaxSteps != nil {
			s.cfg.Agent.MaxSteps = next.Agent.MaxSteps
			s.agent.SetMaxSteps(next.Agent.MaxSteps)
		}
		if provider != nil {
			s.agent.SetProvider(provider)
			s.agent.SetLLMConfig(next.LLM)
			s.SyncContextWindow()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": s.configView()})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// configView 返回脱敏后的配置视图（绝不返回 API Key 明文）。
func (s *Server) configView() map[string]any {
	workDir := ""
	if s.fs != nil {
		workDir = s.fs.Root()
	}
	return map[string]any{
		"provider":     s.cfg.LLM.Provider,
		"permission":   s.executor.Policy().Mode(),
		"base_url":     s.cfg.LLM.BaseURL,
		"model":        s.cfg.LLM.Model,
		"display_name": s.cfg.LLM.DisplayName,
		"max_tokens":   s.cfg.LLM.MaxTokens,
		"max_steps":    s.cfg.Agent.MaxSteps,
		"temperature":  s.cfg.LLM.Temperature,
		// 重试策略：前端设置页可调（次数 / 间隔模式 / 基础间隔）。
		"retry_max_attempts": s.cfg.LLM.MaxAttempts,
		"retry_mode":         s.cfg.LLM.RetryMode,
		"retry_interval_sec": s.cfg.LLM.RetryBackoffMs / 1000,
		"api_key_set":        s.cfg.LLM.APIKey != "",
		"api_key_masked":     s.cfg.MaskedAPIKey(),
		"platform":           platformName(),
		"work_dir":           workDir,
		"tools":              s.registry.Names(),
		// 当前生效模型是否仍在模型库中：删除模型库最后一条后 local.yaml 的
		// 生效配置仍在（对话可用），但界面应提示「库已空、去设置里添加」，
		// 而不是照常显示模型名。
		"model_in_library": func() bool {
			_, ok := s.ModelStore().Find(s.cfg.LLM.Model)
			return ok || strings.TrimSpace(s.cfg.LLM.Model) == ""
		}(),
		// 思考强度分级：随上游协议动态返回（steps 枚举 / range 区间 / none 不支持）
		"thinking": llm.ThinkingSpecFor(s.cfg.LLM.Provider),
	}
}

// treeNode 是文件树节点。
type treeNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	IsDir    bool       `json:"is_dir"`
	Size     int64      `json:"size"`
	Children []treeNode `json:"children,omitempty"`
}

// handleTree 返回工作区文件树（懒加载）。
//
// query 参数 picker=1 是内置目录选择器专用：挑选工作区必须能浏览**工作区外**的
// 目录（如 Termux 的 ~/storage/shared、Linux 桌面的 ~），此时绝对路径不再要求
// 落在工作区内。tree 只暴露条目名/大小、不读内容，且在 requireAuth 之后，
// 越权风险面仅限「列目录名」，与系统对话框任选目录一致。
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	root := s.fs.Root()
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	picker := r.URL.Query().Get("picker") == "1"
	depth := 1
	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 5 {
			depth = n
		}
	}

	target := root
	if rel != "" {
		if filepath.IsAbs(rel) {
			// 绝对路径（内置选择器 browse 传的是 tree 返回的绝对路径）：
			// 默认必须落在工作区内，防越权浏览区外目录；picker 模式放行（见函数注释）。
			if root == "" || !pathWithin(root, rel) {
				if !picker {
					writeJSON(w, http.StatusForbidden, map[string]any{"error": "路径越出工作区范围"})
					return
				}
			}
			target = filepath.Clean(rel)
		} else {
			target = filepath.Join(root, filepath.Clean("/"+rel))
		}
	}
	// picker 模式下连工作区都没选（root=""）且没传 path：回落到用户主目录，
	// 让「选工作区」的入口在任何状态下都有内容可浏览（否则列表永远为空）。
	if target == "" && picker {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			target = home
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "路径不存在"})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不是目录"})
		return
	}

	items, readErr := listTree(target, depth)
	resp := map[string]any{
		"root":  root,
		"path":  target,
		"items": items,
		// parent 供内置选择器渲染「.. 返回上一级」。放在服务端算而不是让前端
		// 切字符串：根目录（/ 与 C:\）、UNC 前缀、结尾分隔符这些规则各平台不同，
		// 前端自己拼会在 Termux/Windows 上错位。已经在根上时返回空串，前端据此隐藏该行。
		"parent": parentDir(target),
	}
	// read_error 与「items 为空」是两回事，必须分开报。
	//
	// 目录 stat 成功但 ReadDir 失败（安卓未授权时的 /storage/emulated/0、
	// Linux 上 root 拥有的目录）原先被 listTree 吞掉 → 回 200 + 空列表 →
	// 前端渲染成「无子目录」。用户看到的是一个**看起来正常、实则读不到任何东西**
	// 的挂载点，唯一的线索（权限）被丢掉了（安卓 2026-10 反馈：弹出来一个空挂载）。
	if readErr != nil {
		resp["read_error"] = describeTreeReadError(target, readErr)
	}
	writeJSON(w, http.StatusOK, resp)
}

// describeTreeReadError 把「目录存在但读不出来」翻译成用户能照做的下一步。
//
// 分两类：
//   - 安卓 Termux 上读手机存储失败 → 分区存储未授权，唯一出路是 termux-setup-storage；
//   - 其他 → 原样带上 errno，但换成中文可读的说法。
func describeTreeReadError(dir string, err error) string {
	if platform.IsTermux() && isStoragePath(dir) {
		return "无法读取手机存储：" + err.Error() +
			"。这是安卓分区存储未授权，请在 Termux 执行 termux-setup-storage 后重试"
	}
	if errors.Is(err, os.ErrPermission) {
		return "无权限读取此目录（" + err.Error() + "）"
	}
	return "无法读取此目录：" + err.Error()
}

// isStoragePath 判断路径是否指向安卓的外部存储（软链 ~/storage/shared、
// /storage/emulated/0、/sdcard 三种写法都算）。
// 三个判据各自独立，不能塞进一个 for 里靠 `p` 复用 ——
// "/sdcard" 只需要前缀匹配，另两个是「路径中任意位置出现」，
// 混在一起写成前缀判断时 ~/storage/shared 会被漏掉（它是 Termux 里的
// 软链路径，不以 /storage/ 开头）。
func isStoragePath(dir string) bool {
	d := filepath.ToSlash(dir)
	return strings.HasPrefix(d, "/storage/") ||
		strings.HasPrefix(d, "/sdcard") ||
		strings.Contains(d, "/storage/shared") ||
		strings.Contains(d, "/storage/emulated/")
}

// parentDir 返回上一级目录；已在根上时返回空串。
func parentDir(dir string) string {
	p := filepath.Dir(dir)
	if p == dir {
		return "" // 根目录的上一级就是自己
	}
	return p
}

// listTree 递归列出目录内容。
//
// 第二返回值是**读取失败**（与「目录为空」严格区分）：调用方必须把它透出，
// 否则权限问题会被渲染成「空目录」，用户无从下手。
// 递归子层失败只跳过该子树，不影响本层 —— 一个子目录读不到
// （典型：Android/data）不该让整个列表失败。
func listTree(dir string, depth int) ([]treeNode, error) {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]treeNode, 0, len(items))
	for _, it := range items {
		info, err := it.Info()
		if err != nil {
			continue
		}
		isDir := it.IsDir()
		// ⚠️ 符号链接要跟到目标再判一次。
		//
		// DirEntry.IsDir() 报的是**链接自身**的类型（DirEntry 走 lstat 语义），
		// 于是「指向目录的软链」IsDir() 为 false，会被归进**文件**列表：
		// 文件选择器里能选中它、能点「选择此文件」，拿回来一个目录路径。
		// 对文件选择器来说这是实打实的错判，所以这里补一次 Stat。
		//
		// 只对符号链接多花一次系统调用 —— 真实目录仍走 IsDir() 快路径。
		if !isDir && it.Type()&os.ModeSymlink != 0 {
			if st, statErr := os.Stat(filepath.Join(dir, it.Name())); statErr == nil {
				isDir = st.IsDir()
			}
		}
		if isDir && skipEntry(it.Name()) {
			continue
		}
		node := treeNode{
			Name:  it.Name(),
			Path:  filepath.Join(dir, it.Name()),
			IsDir: isDir,
			Size:  info.Size(),
		}
		if isDir && depth > 1 {
			// 子层失败留 nil：调用方只关心顶层是否可读。
			node.Children, _ = listTree(filepath.Join(dir, it.Name()), depth-1)
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// pathWithin 词法判断 path 是否位于 root 之内（含 root 自身；不做软链解析，
// tree 浏览属低危面，词法层足够；深度防线在文件工具的 FS.checkScope）。
func pathWithin(root, path string) bool {
	root, path = filepath.Clean(root), filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func skipEntry(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "vendor", ".idea", ".vscode", "__pycache__", ".codeforge":
		return true
	}
	return false
}

// handleSessions 会话的列举 / 新建 / 删除 / 读取 / 重命名 / 归档 / 恢复。
// GET 无 id：返回全部工作区的未归档会话（侧栏分组视图）；
// GET ?archived=1：返回全部已归档会话（设置页归档管理）。
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	hist := s.agent.History()
	ws := s.agent.WorkDir()
	switch r.Method {
	case http.MethodGet:
		if id := r.URL.Query().Get("id"); id != "" {
			sess, ok := hist.Get(id)
			if !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "会话不存在: " + id})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"session": sess})
			return
		}
		archived := r.URL.Query().Get("archived") == "1"
		writeJSON(w, http.StatusOK, map[string]any{"items": hist.List("", archived)})

	case http.MethodPost:
		var body struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		sess, err := hist.Create(ws, body.Title)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "创建会话", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session": sess})

	case http.MethodPatch:
		// PATCH /api/sessions {"id","title"} → 重命名；{"id","archived":true/false} → 归档/恢复；
		// {"id","workspace"} → 更新会话归属的工作区。
		var body struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			Archived  *bool  `json:"archived"`
			Workspace string `json:"workspace"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		if body.Workspace != "" {
			if err := hist.SetWorkspace(body.ID, strings.TrimSpace(body.Workspace)); err != nil {
				writeErr(w, http.StatusInternalServerError, "切换会话所属项目", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if body.Archived != nil {
			var err error
			if *body.Archived {
				err = hist.Archive(body.ID)
			} else {
				err = hist.Unarchive(body.ID)
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "归档或取消归档会话", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if err := hist.Rename(body.ID, strings.TrimSpace(body.Title)); err != nil {
			writeErr(w, http.StatusInternalServerError, "重命名会话", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		if err := hist.Delete(id); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除会话", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleWorkspaces 项目的重命名 / 归档 / 删除。
// 侧栏分组键 = 会话的 workspace 字段（= 磁盘工作区路径，不可变）；
// 「重命名」只改 workspace_names 表里的**显示名**，不动键、不动磁盘。
func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	hist := s.agent.History()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"items": hist.ListWorkspaces()})

	case http.MethodPatch:
		var body struct {
			Workspace string  `json:"workspace"`
			NewName   *string `json:"new_name"` // 重命名分组（显示名）；显式传空串 = 清除自定义名
			Archive   *bool   `json:"archive"`  // true = 归档该组全部会话；false = 恢复（与侧栏「归档」对称）
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		// workspace 允许为空串 = 「未选择工作区」分组，同样可重命名 / 归档 / 恢复。
		// ⚠️ Archive 用指针：区分「没传这个字段」和「显式传 false（= 恢复整个项目）」。
		// 若用裸 bool，`{archive:false}` 会被当成「没传」而落到 400，项目级恢复就没法表达。
		if body.Archive != nil {
			var err error
			if *body.Archive {
				err = hist.ArchiveWorkspace(body.Workspace)
			} else {
				err = hist.UnarchiveWorkspace(body.Workspace)
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "归档或取消归档项目", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		// 用指针区分「没传 new_name」和「传了空串」：后者 = 清除自定义显示名，
		// 前端回落按工作区路径末段显示（不是错误，所以不能和缺失一起走 400）。
		if body.NewName != nil {
			// 重命名 = 只改**显示名**（workspace_names 表）。
			// ⚠️ 绝不顺手改 sessions.workspace / agent 工作目录：那个键就是磁盘路径，
			// 一改项目就失去工作区（文件工具全线报错），历史上这里正是这么坏的。
			if err := hist.SetWorkspaceName(body.Workspace, *body.NewName); err != nil {
				writeErr(w, http.StatusInternalServerError, "重命名项目", err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 new_name 或 archive"})

	case http.MethodDelete:
		// DELETE /api/workspaces?workspace=xxx → 删除整个项目及其全部会话
		ws := r.URL.Query().Get("workspace")
		if err := hist.DeleteWorkspace(ws); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除项目", err)
			return
		}
		// 删除的是当前运行中的项目 → 清空运行态（回到未选择项目）
		if ws == s.agent.WorkDir() {
			if setter, ok := s.fs.(interface{ SetRoot(string) }); ok {
				setter.SetRoot("")
			}
			s.agent.SetWorkDir("")
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleUndo 撤销最近一次文件写入。
func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	path, ok := s.fs.Undo()
	if !ok {
		// 「有路径但撤不了」与「没得撤」是两回事，**文案必须分开**。
		// 前者通常是「改动前的内容过大、没保留下来」；说成「没有可撤销的操作」
		// 是在对用户说假话 —— 明明有一步操作摆在那里。
		msg := "没有可撤销的操作"
		if path != "" {
			msg = "这次改动前的内容过大或已被清理，无法自动还原（改动本身已成功，但没有留副本）"
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"path":      path,
		"remaining": s.fs.UndoDepth(),
	})
}

// handleContextCompress 主动压缩会话的较早历史（上下文面板上的「立即压缩上下文」）。
//
// 状态码分得细，是因为前端要给出不同说法：
//   - 409：点早了（任务正在跑 / 没有可压缩的较早历史）—— 正常提示，不是故障；
//   - 502：摘要这一步真的失败（上游报错）—— 历史未被改动，可以再试一次。
func (s *Server) handleContextCompress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	id := strings.TrimSpace(body.SessionID)
	if id == "" {
		// 面板可能在会话建立前就被打开：退回当前工作区最近更新的那条。
		id = s.agent.History().Latest(s.agent.WorkDir())
	}
	if id == "" {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "当前没有活动会话可压缩"})
		return
	}
	info, err := s.agent.CompressNow(r.Context(), id)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, agent.ErrCompressBusy) || errors.Is(err, agent.ErrCompressNothing) {
			status = http.StatusConflict
		}
		writeErr(w, status, "压缩会话上下文", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session_id": id,
		"summarized": info.Summarized, "added": info.Added,
		"before": info.Before, "after": info.After, "budget": info.Budget,
	})
}

// handleRewind 返回会话的检查点列表，或把工作区文件回滚到某个步骤之前。
//
//	GET  /api/sessions/rewind?id=xxx            → 列出回滚点（按步骤聚合）
//	POST /api/sessions/rewind {id, to_step}     → 回滚文件到 to_step 之前
//
// 与 WS 的 rewind/checkpoints 消息同源（共用 agent 层实现）：
// REST 供设置页/脚本这类非实时的入口使用，WS 供聊天界面即时交互。
func (s *Server) handleRewind(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"steps":     s.agent.CheckpointSteps(id),
			"editables": s.agent.EditableUserMessages(id, 3),
		})

	case http.MethodPost:
		var body struct {
			ID     string `json:"id"`
			ToStep int    `json:"to_step"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		res, err := s.agent.RewindFiles(body.ID, body.ToStep)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "回退文件改动", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleMemory 用户记忆的列举 / 新增 / 更新 / 删除（按当前工作区隔离）。
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"items": s.agent.ListMemories()})

	case http.MethodPost:
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "content 不能为空"})
			return
		}
		if _, err := s.agent.AddMemory(body.Content); err != nil {
			writeErr(w, http.StatusInternalServerError, "添加记忆", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": s.agent.ListMemories()})

	case http.MethodPatch:
		var body struct {
			ID      int64  `json:"id"`
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == 0 || strings.TrimSpace(body.Content) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id 或 content"})
			return
		}
		if err := s.agent.UpdateMemory(body.ID, body.Content); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新记忆", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": s.agent.ListMemories()})

	case http.MethodDelete:
		idStr := r.URL.Query().Get("id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id 无效"})
			return
		}
		if err := s.agent.DeleteMemory(id); err != nil {
			writeErr(w, http.StatusInternalServerError, "删除记忆", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// pluginsPath 返回 plugins.yaml 路径（与配置文件同目录）。
func (s *Server) pluginsPath() string {
	return filepath.Join(s.cfg.ConfigDir(), "plugins.yaml")
}

// handlePlugins MCP/插件管理：列表 / 添加（热加载）/ 启停 / 删除。
// 侧栏 MCP 入口只展示 type=mcp（stdio）与 mcp-http（远程 Streamable HTTP）的条目；
// 列表接口仍返回全部类型（含 endpoint，供前端展示远程端点）。
func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		loaded := map[string]bool{}
		for _, n := range s.registry.Names() {
			loaded[n] = true
		}
		out := make([]map[string]any, 0, len(s.plugins))
		for _, p := range s.plugins {
			// MCP 插件以「插件名.工具名」形式注册，判断是否有任一工具来自该插件
			isLoaded := false
			for n := range loaded {
				if strings.HasPrefix(n, p.Name+".") {
					isLoaded = true
					break
				}
			}
			out = append(out, map[string]any{
				"name": p.Name,
				// display_name 是界面文案（可为空，前端回退到 name）。
				// name 仍原样下发：增删改查、工具路由都以它为准，前端不要拿显示名去定位。
				"display_name": p.Label(),
				"description":  p.Description,
				"type":         p.Type,
				"command":      p.Command,
				"args":         p.Args,
				"endpoint":     p.Endpoint,
				"path":         p.Path,
				"enabled":      p.Enabled && isLoaded,
				"configured":   p.Enabled,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})

	case http.MethodPost:
		// 添加一个 MCP 插件：stdio（type=mcp，需启动命令）或远程 Streamable HTTP
		// （type=mcp-http，需 http/https 绝对 URL 端点），默认启用。
		var body struct {
			Name        string            `json:"name"`
			DisplayName string            `json:"display_name"`
			Type        string            `json:"type"`
			Command     string            `json:"command"`
			Args        []string          `json:"args"`
			Endpoint    string            `json:"endpoint"`
			Description string            `json:"description"`
			Env         map[string]string `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Type = strings.TrimSpace(body.Type)
		if body.Type == "" {
			body.Type = "mcp" // 兼容旧前端：不传类型即 stdio
		}
		body.Command = strings.TrimSpace(body.Command)
		body.Endpoint = strings.TrimSpace(body.Endpoint)
		if body.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名称不能为空"})
			return
		}
		switch body.Type {
		case "mcp":
			if body.Command == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "stdio 类型必须填写启动命令"})
				return
			}
		case "mcp-http":
			if err := validateMCPEndpoint(body.Endpoint); err != nil {
				writeErr(w, http.StatusBadRequest, "校验 MCP 端点", err)
				return
			}
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的类型: " + body.Type + "（可选 mcp / mcp-http）"})
			return
		}
		// 名称唯一性：与现有条目（无论启用与否）判重
		for _, p := range s.plugins {
			if strings.EqualFold(p.Name, body.Name) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "同名插件已存在: " + body.Name})
				return
			}
		}
		desc := strings.TrimSpace(body.Description)
		if desc == "" {
			desc = "MCP 服务"
		}
		p := config.PluginConfig{
			Name: body.Name,
			// 显示名留空时后面 Label() 会回退到 Name，不必在这里编一个。
			DisplayName: strings.TrimSpace(body.DisplayName),
			Type:        body.Type,
			Enabled:     true,
			Description: desc,
			Command:     body.Command,
			Args:        body.Args,
			Endpoint:    body.Endpoint,
			Env:         body.Env,
		}
		if err := s.savePlugin(p); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存插件", err)
			return
		}
		// 增量热加载：只拉起新增插件，不重启其他运行中的 MCP
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": s.reloadPlugin(body.Name)})

	case http.MethodPatch:
		// 启停切换：{"name","enabled"}。增量生效——停用立即卸载目标插件的进程与工具，
		// 启用立即拉起；其他运行中的 MCP 不受影响（不重启）。
		var body struct {
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 name"})
			return
		}
		found := false
		for i := range s.plugins {
			if strings.EqualFold(s.plugins[i].Name, body.Name) {
				s.plugins[i].Enabled = body.Enabled
				found = true
				break
			}
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "插件不存在: " + body.Name})
			return
		}
		if err := config.SavePlugins(s.pluginsPath(), s.plugins); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存插件配置失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warning": s.reloadPlugin(body.Name)})

	case http.MethodDelete:
		name := r.URL.Query().Get("name")
		if name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 name"})
			return
		}
		kept := s.plugins[:0]
		found := false
		for _, p := range s.plugins {
			if strings.EqualFold(p.Name, name) {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if !found {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "插件不存在: " + name})
			return
		}
		s.plugins = kept
		if err := config.SavePlugins(s.pluginsPath(), s.plugins); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存插件配置失败: " + err.Error()})
			return
		}
		// 立即卸载被删插件的进程与工具（增量，不影响其他 MCP；旧实现会残留到重启）
		s.pluginManager.UpdateConfigs(s.plugins)
		s.pluginManager.Unload(name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// validateMCPEndpoint 校验远程 MCP（Streamable HTTP）端点：必须是 http/https 绝对 URL。
// 相对路径或缺少 scheme 会在驱动层以难以理解的传输错误失败，这里提前拦下并给出可读提示。
func validateMCPEndpoint(raw string) error {
	if raw == "" {
		return fmt.Errorf("远程 MCP 必须填写端点 URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("端点 URL 无效: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("端点必须是以 http:// 或 https:// 开头的完整 URL")
	}
	if u.Host == "" {
		return fmt.Errorf("端点 URL 缺少主机名")
	}
	return nil
}

// savePlugin 追加一个插件到 plugins.yaml 并更新内存配置。
func (s *Server) savePlugin(p config.PluginConfig) error {
	s.plugins = append(s.plugins, p)
	if err := config.SavePlugins(s.pluginsPath(), s.plugins); err != nil {
		// 回滚内存，保持配置一致
		s.plugins = s.plugins[:len(s.plugins)-1]
		return fmt.Errorf("保存插件配置失败: %w", err)
	}
	return nil
}

// reloadPlugins 同步最新插件配置到管理器并全量热加载；
// 返回告警信息（空串表示全部成功）。
func (s *Server) reloadPlugins() (warning string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.pluginManager.UpdateConfigs(s.plugins) // 关键：Reload 必须用最新配置，否则启停/新增不生效
	if err := s.pluginManager.Reload(ctx); err != nil {
		return "插件已保存，但热加载失败（重启后生效）：" + err.Error()
	}
	return ""
}

// reloadPlugin 增量热加载单个插件：只启停目标插件的进程与工具，
// 不触碰其他运行中的 MCP（全量 Reload 会重启所有 npx/node 子进程，耗时可达数十秒）。
// 返回告警信息（空串表示成功或无需加载）。
func (s *Server) reloadPlugin(name string) (warning string) {
	s.pluginManager.UpdateConfigs(s.plugins)
	if reason := s.pluginManager.LoadOne(context.Background(), name); reason != "" {
		return "插件已保存，但加载失败（工具不可用）：" + reason
	}
	return ""
}

// handleSkills 技能列表（只读；技能以 SKILL.md 文件为准，直接编辑文件管理）。
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	skills := s.agent.ListSkills()
	out := make([]map[string]any, 0, len(skills))
	for _, sk := range skills {
		display := sk.DisplayName
		if display == "" {
			display = sk.Name // 未配置显示名时回退技能 slug
		}
		out = append(out, map[string]any{
			"name":         sk.Name,
			"display_name": display,
			"description":  sk.Description,
			"triggers":     sk.Triggers,
			"enabled":      sk.Enabled,
			"path":         sk.Path,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
