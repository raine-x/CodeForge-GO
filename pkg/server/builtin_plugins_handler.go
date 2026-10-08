package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"codeforge/config"
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
	case agent.BuiltinPlan.ID:
		return s.cfg.BuiltinPlugins.PlanEnabled()
	case agent.BuiltinGoalMode.ID:
		return s.cfg.BuiltinPlugins.GoalModeEnabled()
	case agent.BuiltinVulnerabilityResearch.ID:
		return s.cfg.BuiltinPlugins.VulnerabilityResearchEnabled()
	case agent.BuiltinReverseAnalysis.ID:
		return s.cfg.BuiltinPlugins.ReverseAnalysisEnabled()
	default:
		return false
	}
}

// setBuiltinEnabled 写回开关状态并持久化到 state.yaml。
func (s *Server) setBuiltinEnabled(id string, on bool) error {
	switch id {
	case agent.BuiltinSkillCreator.ID:
		s.cfg.BuiltinPlugins.SkillCreator = &on
	case agent.BuiltinMultiAgent.ID:
		s.cfg.BuiltinPlugins.MultiAgent = &on
	case agent.BuiltinPlan.ID:
		s.cfg.BuiltinPlugins.Plan = &on
	case agent.BuiltinGoalMode.ID:
		s.cfg.BuiltinPlugins.GoalMode = &on
	case agent.BuiltinVulnerabilityResearch.ID:
		s.cfg.BuiltinPlugins.VulnerabilityResearch = &on
	case agent.BuiltinReverseAnalysis.ID:
		s.cfg.BuiltinPlugins.ReverseAnalysis = &on
	default:
		return nil // 未知插件：仅运行态应用，不落盘
	}
	return s.cfg.SaveState()
}

// builtinSettingDesc 描述插件的一个数值可调项（当前只有目标模式的自循环轮数）。
type builtinSettingDesc struct {
	Label string `json:"label"`
	Value int    `json:"value"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
	Unit  string `json:"unit,omitempty"`
}

// builtinSetting 返回该插件的数值可调项（没有则 nil）。
//
// 当前只有目标模式有；以后再加带数值项的插件，在这里加一条 case 即可，
// 前端是通用渲染器，不用改。
func (s *Server) builtinSetting(id string) *builtinSettingDesc {
	if id != agent.BuiltinGoalMode.ID {
		return nil
	}
	return &builtinSettingDesc{
		Label: "自循环轮数",
		Value: s.cfg.BuiltinPlugins.GoalModeRounds(),
		Min:   1,
		Max:   config.GoalModeMaxRoundsCap,
		Unit:  "轮",
	}
}

// handleBuiltinPlugins 内置插件列表 / 开关（设置 → 插件）。
// PATCH 立即生效：注册/注销工具 + 同步 System Prompt 注入；配置落盘重启后保持。
func (s *Server) handleBuiltinPlugins(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := make([]map[string]any, 0, 4)
		for _, p := range agent.ListBuiltinPlugins() {
			item := map[string]any{
				"id":          p.ID,
				"name":        p.Name,
				"purpose":     p.Purpose,
				"when_to_use": p.WhenToUse,
				"enabled":     s.builtinEnabled(p.ID),
			}
			// 插件可以带一个数值设置项（如目标模式的自循环轮数）。
			// 描述符而不是硬编码：以后再加带数值项的插件，前端一行都不用改。
			if set := s.builtinSetting(p.ID); set != nil {
				item["setting"] = set
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})

	case http.MethodPatch:
		var body struct {
			ID string `json:"id"`
			// ⚠️ 两个字段都用指针：Go 的零值分不清「没传」与「传了零值」。
			// 早先用值类型时，只 PATCH {id, value} 改轮数会把 Enabled 读成 false，
			// 于是**改一下轮数就把插件悄悄关了** —— 而界面上开关还亮着。
			Enabled *bool `json:"enabled"`
			Value   *int  `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		if body.Enabled == nil && body.Value == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "没有可更新的字段"})
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
		if body.Value != nil {
			if s.builtinSetting(body.ID) == nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该插件没有可调项: " + body.ID})
				return
			}
			s.cfg.BuiltinPlugins.GoalModeMaxRounds = *body.Value
			// 立刻同步进工具：轮数写在 schema 与 Description 里，
			// 不同步的话模型会按旧数字规划，界面上显示的也是旧值。
			if s.builtinApply != nil {
				s.builtinApply(body.ID, s.builtinEnabled(body.ID))
			}
		}
		if body.Enabled != nil {
			if err := s.setBuiltinEnabled(body.ID, *body.Enabled); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存配置失败: " + err.Error()})
				return
			}
			if s.builtinApply != nil {
				s.builtinApply(body.ID, *body.Enabled) // 运行时立即生效（注册/注销工具 + 提示词开关）
			}
		} else {
			// 只改了数值也要落盘，否则重启后回默认值。
			if err := s.cfg.SaveState(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存配置失败: " + err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": s.builtinEnabled(body.ID)})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
