package server

import (
	"encoding/json"
	"net/http"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/logx"
)

// SetSubagentApply 注入子智能体设置的运行时应用钩子（main.go 实现）：
// 把最新的 config.subagents 派生成策略推给 Agent 与委派工具。
func (s *Server) SetSubagentApply(fn func()) { s.subagentApply = fn }

// handleSubagentPrefs 查看 / 设置子智能体（多智能体协作）策略。设置 → 子智能体。
//
//	GET  → {enabled, max_concurrent, max_allowed, max_steps, max_steps_allowed,
//	       steps_inherit, allow_write, allow_delete, allow_memory}
//	POST → 传哪些字段就改哪些（指针语义），热应用并写回 config/local.yaml
//
// enabled 与内置插件开关是同一真源（builtin_plugins.multi_agent）：这里改它，
// 走的也是同一套「注册/注销工具 + 同步提示词注入」逻辑，两个入口不会互相矛盾。
func (s *Server) handleSubagentPrefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.subagentPrefsView())

	case http.MethodPost:
		var body struct {
			Enabled       *bool `json:"enabled"`
			MaxConcurrent *int  `json:"max_concurrent"`
			MaxSteps      *int  `json:"max_steps"`
			AllowWrite    *bool `json:"allow_write"`
			AllowDelete   *bool `json:"allow_delete"`
			AllowMemory   *bool `json:"allow_memory"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}

		// 总开关：与内置插件页共用同一字段与同一套运行时应用逻辑。
		if body.Enabled != nil {
			on := *body.Enabled
			s.cfg.BuiltinPlugins.MultiAgent = &on
			if s.builtinApply != nil {
				s.builtinApply(agent.BuiltinMultiAgent.ID, on)
			}
		}
		if body.MaxConcurrent != nil {
			// 越界值不报错而是收敛：设置页范围有限，正常不会越界；
			// 手改 local.yaml 写了 99 时按硬上限处理，比拒绝保存更不容易卡住。
			s.cfg.Subagents.MaxConcurrent = *body.MaxConcurrent
		}
		if body.MaxSteps != nil {
			// 0 = 继承主 loop 的 max_steps；负数按 0 处理（同样是「继承」）。
			n := *body.MaxSteps
			if n < 0 {
				n = 0
			}
			s.cfg.Subagents.MaxSteps = n
		}
		if body.AllowWrite != nil {
			s.cfg.Subagents.AllowWrite = body.AllowWrite
		}
		if body.AllowDelete != nil {
			s.cfg.Subagents.AllowDelete = body.AllowDelete
		}
		if body.AllowMemory != nil {
			s.cfg.Subagents.AllowMemory = body.AllowMemory
		}

		if err := s.cfg.SaveState(); err != nil {
			logx.Warnf("子智能体设置已生效但写回运行状态失败（重启后需重新设置）: %v", err)
		}
		if s.subagentApply != nil {
			s.subagentApply()
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": s.subagentPrefsView()})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// subagentPrefsView 汇总子智能体的当前设置（并发返回收敛后的实际值，而非原始配置）。
func (s *Server) subagentPrefsView() map[string]any {
	return map[string]any{
		"enabled":        s.cfg.BuiltinPlugins.MultiAgentEnabled(),
		"max_concurrent": s.cfg.SubagentMaxConcurrent(),
		"max_allowed":    config.SubagentConcurrencyCap,
		// max_steps 原样回显（0 = 继承），steps_effective 给「实际会用多少」，
		// 前端据此把继承态显示成「跟随主循环（当前 N 步）」。
		"max_steps":         s.cfg.Subagents.MaxSteps,
		"max_steps_allowed": config.SubagentStepCap,
		"steps_inherit":     s.cfg.Subagents.MaxSteps <= 0,
		"steps_effective":   s.cfg.SubagentMaxSteps(s.cfg.Agent.MaxSteps),
		"allow_write":       s.cfg.SubagentAllowWrite(),
		"allow_delete":      s.cfg.SubagentAllowDelete(),
		"allow_memory":      s.cfg.SubagentAllowMemory(),
	}
}
