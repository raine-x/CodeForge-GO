package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeforge/config"
	"codeforge/pkg/llm"
)

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
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
		if dir := s.cfg.ConfigDir(); dir != "" {
			_ = s.cfg.Save(filepath.Join(dir, "local.yaml"))
		}
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
			Provider    string   `json:"provider"`
			BaseURL     *string  `json:"base_url"`
			APIKey      string   `json:"api_key"`
			Model       string   `json:"model"`
			MaxTokens   int      `json:"max_tokens"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败: " + err.Error()})
			return
		}

		if strings.TrimSpace(body.Provider) != "" {
			s.cfg.LLM.Provider = strings.TrimSpace(body.Provider)
		}
		if body.BaseURL != nil {
			s.cfg.LLM.BaseURL = strings.TrimSpace(*body.BaseURL)
		}
		// 空 api_key 表示「保持不变」，避免前端回填掩码值覆盖真实 Key。
		if strings.TrimSpace(body.APIKey) != "" {
			s.cfg.LLM.APIKey = strings.TrimSpace(body.APIKey)
		}
		if strings.TrimSpace(body.Model) != "" {
			s.cfg.LLM.Model = strings.TrimSpace(body.Model)
		}
		if body.MaxTokens > 0 {
			s.cfg.LLM.MaxTokens = body.MaxTokens
		}
		if body.Temperature != nil {
			s.cfg.LLM.Temperature = *body.Temperature
		}

		if err := s.rebuildProvider(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if dir := s.cfg.ConfigDir(); dir != "" {
			_ = s.cfg.Save(filepath.Join(dir, "local.yaml"))
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
		"provider":       s.cfg.LLM.Provider,
		"permission":     s.executor.Policy().Mode(),
		"base_url":       s.cfg.LLM.BaseURL,
		"model":          s.cfg.LLM.Model,
		"display_name":   s.cfg.LLM.DisplayName,
		"max_tokens":     s.cfg.LLM.MaxTokens,
		"temperature":    s.cfg.LLM.Temperature,
		"api_key_set":    s.cfg.LLM.APIKey != "",
		"api_key_masked": s.cfg.MaskedAPIKey(),
		"platform":       platformName(),
		"work_dir":       workDir,
		"tools":          s.registry.Names(),
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
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	root := s.fs.Root()
	rel := strings.TrimSpace(r.URL.Query().Get("path"))
	depth := 1
	if d := r.URL.Query().Get("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil && n > 0 && n <= 5 {
			depth = n
		}
	}

	target := root
	if rel != "" {
		target = filepath.Join(root, filepath.Clean("/"+rel))
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

	writeJSON(w, http.StatusOK, map[string]any{
		"root":  root,
		"path":  target,
		"items": listTree(target, depth),
	})
}

// listTree 递归列出目录内容。
func listTree(dir string, depth int) []treeNode {
	items, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]treeNode, 0, len(items))
	for _, it := range items {
		info, err := it.Info()
		if err != nil {
			continue
		}
		if it.IsDir() && skipEntry(it.Name()) {
			continue
		}
		node := treeNode{
			Name:  it.Name(),
			Path:  filepath.Join(dir, it.Name()),
			IsDir: it.IsDir(),
			Size:  info.Size(),
		}
		if it.IsDir() && depth > 1 {
			node.Children = listTree(filepath.Join(dir, it.Name()), depth-1)
		}
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	return out
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
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if err := hist.Rename(body.ID, strings.TrimSpace(body.Title)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleWorkspaces 工作区分组的重命名 / 归档。
// 侧栏分组键 = 会话的 workspace 字段；重命名 = 批量改写分组键（仅影响分组显示与数据归属）。
func (s *Server) handleWorkspaces(w http.ResponseWriter, r *http.Request) {
	hist := s.agent.History()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"items": hist.ListWorkspaces()})

	case http.MethodPatch:
		var body struct {
			Workspace string `json:"workspace"`
			NewName   string `json:"new_name"` // 重命名分组
			Archive   bool   `json:"archive"`  // 归档该组全部会话
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		// workspace 允许为空串 = 「未选择工作区」分组，同样可重命名 / 归档。
		if body.Archive {
			if err := hist.ArchiveWorkspace(body.Workspace); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if name := strings.TrimSpace(body.NewName); name != "" {
			if err := hist.RenameWorkspace(body.Workspace, name); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			// 当前工作区被重命名 → 同步运行态，避免新会话仍挂旧键
			if body.Workspace == s.agent.WorkDir() {
				s.agent.SetWorkDir(name)
				s.cfg.Agent.WorkDir = name
				if dir := s.cfg.ConfigDir(); dir != "" {
					_ = s.cfg.Save(filepath.Join(dir, "local.yaml"))
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 new_name 或 archive"})

	case http.MethodDelete:
		// DELETE /api/workspaces?workspace=xxx → 删除整个项目及其全部会话
		ws := r.URL.Query().Get("workspace")
		if err := hist.DeleteWorkspace(ws); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "没有可撤销的操作"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"path":      path,
		"remaining": s.fs.UndoDepth(),
	})
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
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
// 侧栏 MCP 入口只展示 type=mcp 的条目；列表接口仍返回全部类型。
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
				"name":        p.Name,
				"description": p.Description,
				"type":        p.Type,
				"command":     p.Command,
				"args":        p.Args,
				"enabled":     p.Enabled && isLoaded,
				"configured":  p.Enabled,
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": out})

	case http.MethodPost:
		// 添加一个 MCP 插件：{"name","command","args","description","env"}，默认启用。
		var body struct {
			Name        string            `json:"name"`
			Command     string            `json:"command"`
			Args        []string          `json:"args"`
			Description string            `json:"description"`
			Env         map[string]string `json:"env"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		body.Name = strings.TrimSpace(body.Name)
		body.Command = strings.TrimSpace(body.Command)
		if body.Name == "" || body.Command == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名称和启动命令不能为空"})
			return
		}
		// 名称唯一性：与现有条目（无论启用与否）判重
		for _, p := range s.plugins {
			if strings.EqualFold(p.Name, body.Name) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "同名插件已存在: " + body.Name})
				return
			}
		}
		p := config.PluginConfig{
			Name:        body.Name,
			Type:        "mcp",
			Enabled:     true,
			Description: strings.TrimSpace(body.Description),
			Command:     body.Command,
			Args:        body.Args,
			Env:         body.Env,
		}
		if err := s.savePlugin(p); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
		out = append(out, map[string]any{
			"name":        sk.Name,
			"description": sk.Description,
			"triggers":    sk.Triggers,
			"enabled":     sk.Enabled,
			"path":        sk.Path,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
