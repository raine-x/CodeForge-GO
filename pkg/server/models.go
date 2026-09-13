package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codeforge/config"
)

// modelTestReq 是测试连接请求。
type modelTestReq struct {
	Protocol string `json:"protocol"` // openai | anthropic
	BaseURL  string `json:"base_url"`
	Model    string `json:"model"`
	KeySrc   string `json:"key_source"` // env | plain
	KeyName  string `json:"key_name"`   // 环境变量名
	KeyValue string `json:"key_value"`  // 明文 KEY
}

// resolveKey 依表单来源解析 API Key（env 变量 → 明文）。
// 空串表示表单没给出可用密钥，调用方再决定是否回退模型库。
func (r modelTestReq) resolveKey() string {
	if r.KeySrc == "env" {
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(r.KeyName)))
	}
	return strings.TrimSpace(r.KeyValue)
}

// handleModelTest 测试连接：直接使用表单当前填写的内容（协议/地址/模型 id/密钥）
// 向上游发送一次最小请求。所见即所测——不读模型库、不引用「生效配置」、不做重试，
// 失败原因按状态码映射为直白的中文说明，避免任何混淆。
func (s *Server) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req modelTestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}

	key := strings.TrimSpace(req.resolveKey())
	if key == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "API Key 为空：请填写密钥或设置环境变量"})
		return
	}
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请求地址不能为空"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "模型 id 不能为空"})
		return
	}

	// 单次请求，30s 超时（覆盖慢网关首 token）。
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// 与对话链路同走流式：推理型模型（如 glm-5.3）非流式要等完整生成才返回，
	// 测试常超时；stream:true 下首块秒回，判定真实可用。
	payload, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	headers := map[string]string{"Content-Type": "application/json"}
	var apiURL string
	if req.Protocol == "anthropic" {
		apiURL = base + "/v1/messages"
		headers["x-api-key"] = key
		headers["anthropic-version"] = "2023-06-01"
	} else {
		apiURL = base + "/chat/completions"
		headers["Authorization"] = "Bearer " + key
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请求超时（30s）：上游响应过慢，请稍后重试或检查网络"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请求失败：" + err.Error()})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 流式响应：2xx 后再等首个数据块（最多 8s），确认上游真的开始输出。
		first := make(chan int, 1)
		go func() {
			buf := make([]byte, 2048)
			n, _ := io.ReadAtLeast(resp.Body, buf, 1)
			first <- n
		}()
		var n int
		select {
		case n = <-first:
		case <-time.After(8 * time.Second):
		}
		if n > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": resp.StatusCode})
		} else {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "上游已接受请求但迟迟未开始输出，请稍后重试"})
		}
		return
	}

	// 解析错误体详情，并映射为直白的中文原因。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var body struct {
		Error any `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	detail := ""
	if s, ok := body.Error.(string); ok && s != "" {
		detail = s
	} else if body.Error != nil {
		if m, err := json.Marshal(body.Error); err == nil {
			detail = string(m)
		}
	}

	var reason string
	switch resp.StatusCode {
	case 401:
		reason = "密钥无效或已过期（401）——请检查 API Key"
	case 403:
		reason = "该密钥无权访问模型 " + model + "（403）——请到网关/平台控制台确认此 Key 的模型权限"
	case 404:
		reason = "地址或模型 id 不存在（404）——请检查请求地址与模型 id"
	case 400:
		reason = "请求被拒绝（400）——通常是模型 id 或请求参数不符，请核对模型 id"
	case 429:
		reason = "上游限流（429）——请稍后重试"
	case 408, 500, 502, 503, 504:
		reason = "上游瞬时故障（HTTP " + strconv.Itoa(resp.StatusCode) + "）——请稍后重试"
	default:
		reason = "上游返回 HTTP " + strconv.Itoa(resp.StatusCode)
	}
	if detail != "" && !strings.Contains(detail, reason) {
		reason += "：" + detail
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": resp.StatusCode, "error": reason})
}

// modelApplyReq 是应用模型配置请求。
// 单链路改造后 apply 只接受 {model, key_value?}：
// 其余字段一律以模型库中的条目为准，密钥明文不出服务端。
// key_value 仅在模型库条目没有可用密钥、而用户在表单里现填了一个时才有意义。
type modelApplyReq struct {
	Model    string `json:"model"`
	KeyValue string `json:"key_value"` // 可选：仅当库里该模型 key_set=false 时采用
}

// resolveEntryKey 解析一条模型配置的实际密钥：env 变量 → 明文 → 本次请求补充值。
func (s *Server) resolveEntryKey(m config.ModelEntry, supplied string) string {
	if strings.EqualFold(m.KeySource, "env") && strings.TrimSpace(m.KeyName) != "" {
		if v := strings.TrimSpace(os.Getenv(strings.TrimSpace(m.KeyName))); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(m.KeyValue); v != "" {
		return v
	}
	return strings.TrimSpace(supplied)
}

// handleModelList 返回脱敏后的模型库（明文 key 不下发，只有 key_set）。
// 唯一例外：当前「已应用/生效」的模型会附带 key_plain 明文，便于设置页
// 直接显示/查看正在使用的密钥（用户本就拥有该密钥）。
func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	active := s.cfg.LLM.Model
	models := s.ModelStore().Sanitized()
	for _, m := range models {
		if id, _ := m["id"].(string); id == active {
			if entry, ok := s.ModelStore().Find(id); ok {
				if v := s.resolveEntryKey(entry, ""); v != "" {
					m["key_plain"] = v
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models": models,
		"active": active,
	})
}

// modelSaveReq 是保存模型的请求体。不复用 ModelEntry 直接解码：
// 其 KeyValue 标了 json:"-"（防止明文 key 序列化下发），保存接口需要接收它。
type modelSaveReq struct {
	config.ModelEntry
	KeyValue string `json:"key_value"` // 明文 Key；编辑时留空 = 保持库中旧值
}

// handleModelSave 新增 / 更新一条模型（按 id 判重）。
// 编辑时 key_value 留空表示「保持库里已存的密钥不变」。
func (s *Server) handleModelSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req modelSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	m := req.ModelEntry
	m.KeyValue = strings.TrimSpace(req.KeyValue)
	m.ID = strings.TrimSpace(m.ID)
	// 入口先归一化协议（custom→openai 等）：热切换写 LLMConfig 用的是这里的 m，
	// 不能依赖 Upsert（其内部归一化发生在副本上）。
	m.Protocol = config.NormalizeModelProtocol(m.Protocol)
	if m.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型 id 不能为空"})
		return
	}
	store := s.ModelStore()
	// 编辑已有条目且本次未填明文 key → 保留库里的旧值（防止前端拿不到明文而清掉密钥）。
	if strings.TrimSpace(m.KeyValue) == "" {
		if old, ok := store.Find(m.ID); ok {
			m.KeyValue = old.KeyValue
		}
	}
	if err := store.Upsert(m); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := store.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存模型库失败: " + err.Error()})
		return
	}
	// 编辑的是当前生效模型 → 实时热切换（与「应用」等价），改动即刻生效，
	// 免去用户再点一次「应用」。非当前模型只入库，待「应用」时再切换。
	if m.ID == s.cfg.LLM.Model {
		key := s.resolveEntryKey(m, m.KeyValue)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "该模型没有可用密钥（环境变量未设置且库里无明文），请先补填再保存"})
			return
		}
		s.cfg.LLM.DisplayName = m.DisplayName()
		s.cfg.LLM.BaseURL = strings.TrimSpace(m.BaseURL)
		s.cfg.LLM.Provider = m.Protocol
		s.cfg.LLM.APIKey = key
		if m.CtxOut > 0 {
			s.cfg.LLM.MaxTokens = m.CtxOut
		}
		if err := s.rebuildProvider(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if dir := s.cfg.ConfigDir(); dir != "" {
			_ = s.cfg.Save(filepath.Join(dir, "local.yaml"))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": store.Sanitized()})
}

// handleModelDelete 从模型库删除一条；若删除的是当前生效模型，仅从列表移除，
// 不动运行配置（保持「当前会话不中断」）。
func (s *Server) handleModelDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	store := s.ModelStore()
	if !store.Delete(body.Model) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "模型不在列表中: " + body.Model})
		return
	}
	if err := store.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存模型库失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": store.Sanitized()})
}

// handleModelApply 把模型库中的某一条应用为「当前生效」配置并热切换 Provider。
// 流程：从库里找条目 → 解析密钥（env/明文/请求补充）→ 覆写 s.cfg.LLM →
// rebuildProvider → 写回 local.yaml（与旧版行为一致：当前生效配置落盘）。
func (s *Server) handleModelApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req modelApplyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型 id 不能为空"})
		return
	}
	store := s.ModelStore()
	m, ok := store.Find(model)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "模型不在库中，请先在「设置 → 模型」里添加: " + model,
		})
		return
	}
	key := s.resolveEntryKey(m, req.KeyValue)
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "该模型没有可用密钥（环境变量未设置且库里无明文），请在设置中补填",
		})
		return
	}

	llm := &s.cfg.LLM
	llm.Model = m.ID
	llm.DisplayName = m.DisplayName()
	llm.BaseURL = strings.TrimSpace(m.BaseURL)
	llm.Provider = m.Protocol
	llm.APIKey = key
	// 模型条目里设置的「输出上限」（tokens，无单位直填）落到实际请求的 max_tokens；
	// 未设置（0）时保留现有配置值。
	if m.CtxOut > 0 {
		llm.MaxTokens = m.CtxOut
	}
	if err := s.rebuildProvider(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if dir := s.cfg.ConfigDir(); dir != "" {
		_ = s.cfg.Save(filepath.Join(dir, "local.yaml"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": s.configView()})
}
