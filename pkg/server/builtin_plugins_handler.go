package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"codeforge/pkg/agent"
)

// SetBuiltinPluginApply 注入内置插件开关的运行时应用钩子：
// on=true 注册该插件工具，on=false 注销；同时同步 Agent 的提示词注入开关。
// 由 main.go 用闭包实现（持有 registry / fsys / agent 的具体依赖）。
func (s *Server) SetBuiltinPluginApply(fn func(id string, on bool)) { s.builtinApply = fn }

// builtinEnabled 返回内置插件当前启用状态（配置为唯一权威）。
func (s *Server) builtinEnabled(id string) bool {
	switch id {
	case agent.BuiltinSkillCreator.ID:
		return s.cfg.BuiltinPlugins.SkillCreatorEnabled()
	case agent.BuiltinMultiAgent.ID:
		return s.cfg.BuiltinPlugins.MultiAgentEnabled()
	default:
		return false
	}
}

// setBuiltinEnabled 写回开关状态并持久化到 local.yaml。
func (s *Server) setBuiltinEnabled(id string, on bool) error {
	switch id {
	case agent.BuiltinSkillCreator.ID:
		s.cfg.BuiltinPlugins.SkillCreator = &on
	case agent.BuiltinMultiAgent.ID:
		s.cfg.BuiltinPlugins.MultiAgent = &on
	default:
		return nil // 未知插件：仅运行态应用，不落盘
	}
	if dir := s.cfg.ConfigDir(); dir != "" {
		return s.cfg.Save(dir + "/local.yaml")
	}
	return nil
}

// handleBuiltinPlugins 内置插件列表 / 开关（设置 → 插件）。
// PATCH 立即生效：注册/注销工具 + 同步 System Prompt 注入；配置落盘重启后保持。
func (s *Server) handleBuiltinPlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := make([]map[string]any, 0, 4)
		for _, p := range agent.ListBuiltinPlugins() {
			items = append(items, map[string]any{
				"id":          p.ID,
				"name":        p.Name,
				"purpose":     p.Purpose,
				"when_to_use": p.WhenToUse,
				"enabled":     s.builtinEnabled(p.ID),
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})

	case http.MethodPatch:
		var body struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		known := false
		for _, p := range agent.ListBuiltinPlugins() {
			if p.ID == body.ID {
				known = true
				break
			}
		}
		if !known {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "未知内置插件: " + body.ID})
			return
		}
		if err := s.setBuiltinEnabled(body.ID, body.Enabled); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存配置失败: " + err.Error()})
			return
		}
		if s.builtinApply != nil {
			s.builtinApply(body.ID, body.Enabled) // 运行时立即生效（注册/注销工具 + 提示词开关）
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
