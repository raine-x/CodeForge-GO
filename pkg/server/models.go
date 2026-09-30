package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"codeforge/config"
	"codeforge/pkg/errs"
	"codeforge/pkg/logx"
)

// modelTestReq 是测试连接请求。
//
// 自供应商机制引入后，BaseURL / Protocol / Key* 都可以留空 —— 只要给了
// ProviderID，服务端会从供应商补齐（见 applyProviderDefaults），
// 前端因此不必把供应商上的地址和密钥再抄一遍。
type modelTestReq struct {
	ProviderID string `json:"provider_id"` // 归属供应商；给了就不必重复填下面的连接信息
	Protocol   string `json:"protocol"`    // openai | anthropic
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	KeySrc     string `json:"key_source"` // env | plain
	KeyName    string `json:"key_name"`   // 环境变量名
	KeyValue   string `json:"key_value"`  // 明文 KEY
	// ProbeContext 为真时，连通测试通过后继续逐档探测输入上下文上限。
	// 单独一个开关而不是无条件跑：探测会向上游送一份大输入（成功那一档会真实计费），
	// 用户可能只想确认「密钥和地址对不对」。
	ProbeContext bool `json:"probe_context"`
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
	req = s.applyProviderDefaults(req) // 选定了供应商 → 地址/协议/密钥由它补齐

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

	// 单次请求，超时覆盖慢网关首 token（与模型列表拉取共用同一档，见 upstreamTimeout）。
	ctx, cancel := context.WithTimeout(r.Context(), upstreamTimeout)
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
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "error": errs.FriendlyOr("测试模型连接", err)})
		return
	}
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			// 与 model_discover 同一口径：连不通和上游慢都会走到这里，别只报「上游慢」
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": fmt.Sprintf(
				"请求超时（%s）：未能连上 %s。既可能是上游响应慢，也可能是当前网络到该地址不通"+
					"（域名能解析、但 TCP 连不上，境外服务在部分网络下就是这样）。"+
					"请先用浏览器或 curl 确认该地址可达，再重试", upstreamTimeout, httpReq.URL.Host)})
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
			extra := map[string]any{"ok": true, "status": resp.StatusCode}
			if req.ProbeContext {
				// 基本连通性 OK 才值得探窗口：连不通时探测只会把无关错误解释成「窗口小」。
				// 探测另起一份 context（沿用客户端断开即取消），它比单次测试慢得多。
				pctx, pcancel := context.WithTimeout(r.Context(), probeTotalTimeout)
				out := probeContextWindow(pctx, http.DefaultClient, apiURL, headers, req.Protocol, model)
				pcancel()
				extra["probe"] = out
				if out.CtxIn > 0 {
					extra["ctx_in"] = out.CtxIn
				}
			}
			writeJSON(w, http.StatusOK, extra)
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
	// SessionID 用于「上下文护栏」：切换模型时校验当前会话的原始历史
	// 是否已经超过目标模型的上下文窗口（见 handleModelApply）。
	SessionID string `json:"session_id"`
}

// resolveEntryKey 解析一条模型配置的实际密钥：env 变量 → 明文 → 本次请求补充值。
//
// 先经供应商补齐：条目上留空即用供应商的密钥，这样「同一个供应商下换模型 id」
// 完全不必碰密钥。
func (s *Server) resolveEntryKey(m config.ModelEntry, supplied string) string {
	e := s.resolveModel(m)
	if strings.EqualFold(e.KeySource, "env") && strings.TrimSpace(e.KeyName) != "" {
		if v := strings.TrimSpace(os.Getenv(strings.TrimSpace(e.KeyName))); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(e.KeyValue); v != "" {
		return v
	}
	return strings.TrimSpace(supplied)
}

// modelLibraryPayload 组装「模型库 + 供应商库」的脱敏视图。
//
// 模型条目的明文 key 不下发，只有 key_set 布尔。例外有两处（都是为了设置页能回显）：
//   - 当前「已应用/生效」的模型附带 key_plain；
//   - **每个供应商**都附带 key_plain —— 供应商页是用来核对/改密钥的地方，
//     只给 active 那个回填的话，切到别的供应商密钥框就是空的，
//     用户会以为「保存的 APIKEY 丢了」（2026-09-20 实际反馈）。
//
// /api/models/list 与 /api/providers/list 共用这一份载荷。两个入口曾经各拼一份，
// 供应商那个漏掉了 active 与 key_plain —— 前端一旦改走那个入口，供应商页的
// 「查看密钥」就会静默失效，且不报任何错。统一到这里，避免再次分叉。
func (s *Server) modelLibraryPayload() map[string]any {
	active := s.cfg.LLM.Model
	ps := s.ProviderStore()
	models := s.ModelStore().SanitizedResolved(ps)
	providers := ps.Sanitized()

	for _, m := range models {
		id, _ := m["id"].(string)
		if id != active {
			continue
		}
		if entry, ok := s.ModelStore().Find(id); ok {
			if v := s.resolveEntryKey(entry, ""); v != "" {
				m["key_plain"] = v
			}
		}
	}

	// 供应商明文 key：**每个都回填**，不再只给「当前生效模型所属」那一个。
	//
	// 早先只回填 active 那一个，结果是：切到别的供应商，密钥框就是空的 ——
	// 用户看到的现象是「已经保存的供应商，下次打开不显示 APIKEY 了」，以为没存上。
	// 供应商页本来就是用来核对/修改密钥的地方，全部回填才符合预期。
	//
	// 暴露面没有变大：这份载荷只发给同源的本地页面，密钥框是 type=password
	// （默认掩码，点眼睛才显示明文），所以截图里也不会直接露出密钥。
	for _, pv := range providers {
		id, _ := pv["id"].(string)
		p, ok := ps.Find(id)
		if !ok {
			continue
		}
		if v := providerKey(p); v != "" {
			pv["key_plain"] = v
		}
	}
	return map[string]any{
		"models":    models,
		"providers": providers,
		"active":    active,
	}
}

// handleModelList 返回脱敏后的模型库与供应商库。
func (s *Server) handleModelList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.modelLibraryPayload())
}

// modelSaveReq 是保存模型的请求体。不复用 ModelEntry 直接解码：
// 其 KeyValue 标了 json:"-"（防止明文 key 序列化下发），保存接口需要接收它。
type modelSaveReq struct {
	config.ModelEntry
	KeyValue string `json:"key_value"` // 明文 Key；编辑时留空 = 保持库中旧值
	// InheritKey 为真表示「不要保留条目上原有的密钥覆盖，改用供应商的」。
	// 前端在选定了供应商、且用户没碰过密钥输入框时置真 —— 否则条目上历史遗留的
	// 自带密钥会一直压着供应商的密钥，用户永远摘不掉这个覆盖。
	InheritKey bool `json:"inherit_key"`
}

// handleModelSave 新增 / 更新一条模型（按 id 判重）。
//
// 归属供应商（provider_id）非空时，base_url / protocol / 密钥三件套都可以留空，
// 表示「继承供应商」—— 这正是「同一个供应商下加模型只填 id」的实现方式。
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
	m.ProviderID = strings.TrimSpace(m.ProviderID)
	// 入口先归一化协议（custom→openai 等）：热切换写 LLMConfig 用的是这里的 m，
	// 不能依赖 Upsert（其内部归一化发生在副本上）。留空 = 继承供应商，保持空。
	if strings.TrimSpace(m.Protocol) != "" {
		m.Protocol = config.NormalizeModelProtocol(m.Protocol)
	}
	if m.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "模型 id 不能为空"})
		return
	}
	ps := s.ProviderStore()
	// 归属供应商必须真实存在，否则条目会静默变成「没有地址也没有密钥」。
	if m.ProviderID != "" {
		if _, ok := ps.Find(m.ProviderID); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "供应商不存在: " + m.ProviderID})
			return
		}
	}
	store := s.ModelStore()
	// 编辑已有条目且本次未填明文 key → 保留库里的旧值（防止前端拿不到明文而清掉密钥）。
	// 例外：本次声明「继承供应商密钥」时，条目上的覆盖值必须清掉。
	if !req.InheritKey && strings.TrimSpace(m.KeyValue) == "" {
		if old, ok := store.Find(m.ID); ok {
			m.KeyValue = old.KeyValue
		}
	}
	if err := store.Upsert(m); err != nil {
		writeErr(w, http.StatusBadRequest, "保存模型", err)
		return
	}
	if err := store.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存模型库失败: " + err.Error()})
		return
	}
	// 编辑的是当前生效模型 → 实时热切换（与「应用」等价），改动即刻生效，
	// 免去用户再点一次「应用」。非当前模型只入库，待「应用」时再切换。
	if m.ID == s.cfg.LLM.Model {
		if err := s.applyModelEntry(m, m.KeyValue); err != nil {
			writeErr(w, http.StatusBadRequest, "保存模型", err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"models":    store.SanitizedResolved(ps),
		"providers": ps.Sanitized(),
	})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"models": store.SanitizedResolved(s.ProviderStore()),
	})
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

	// 上下文装不下时**先压缩，再切换** —— 不要拦下来让用户自己去处理。
	//
	// 早先这里直接返回 409 让用户「先在当前模型下压缩/收尾，或新建会话再切换」，
	// 用户的原话是「不要直接截断」（2026-09-21）。
	// 压缩是幂等的、且不改变用户可见的历史（只影响送模视图），
	// 所以完全可以在切换前自动压一次，把选择权还给用户。
	//
	// 判定口径用**压缩后的实际送模量**，不是原始历史 —— 否则刚压好的会话
	// 也会被判成超窗，用户会遇到「明明压过了，一切换又说超限」。
	if m.CtxIn > 0 && req.SessionID != "" {
		if sess, found := s.agent.History().Get(req.SessionID); found && sess != nil {
			if over := s.agent.SessionOverflowFor(sess, m.CtxIn); over > 0 {
				// 目标模型窗口更小 → 先按它的窗口压一次。
				// CompressNow 内部会按当前生效模型的预算做摘要；压完再复核一次，
				// 仍然放不下才如实拒绝（此时确实没有别的办法）。
				if _, err := s.agent.CompressNow(r.Context(), req.SessionID); err != nil {
					logx.Warnf("会话=%s 切换前自动压缩失败：%v", req.SessionID, err)
				}
				if sess2, ok2 := s.agent.History().Get(req.SessionID); ok2 && sess2 != nil {
					if over2 := s.agent.SessionOverflowFor(sess2, m.CtxIn); over2 > 0 {
						writeJSON(w, http.StatusConflict, map[string]any{
							"ok": false,
							"error": fmt.Sprintf(
								"已尝试压缩上下文，但仍放不进该模型的窗口（压缩后约 %s tokens，"+
									"模型上限 %s tokens）。请新建会话后再切换，或换一个窗口更大的模型。",
								humanTokens(s.agent.RequestViewTokens(sess2)),
								humanTokens(m.CtxIn)),
							"code": "context_overflow",
						})
						return
					}
				}
			}
		}
	}

	if err := s.applyModelEntry(m, req.KeyValue); err != nil {
		writeErr(w, http.StatusBadRequest, "应用模型", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": s.configView()})
}

// humanTokens 把 token 数格式化成易读单位（12.3k / 1.2M），供提示文案使用。
func humanTokens(n int) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fM", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// applyModelEntry 把一条**已入库**的模型条目设为「当前生效」配置并热切换 Provider。
//
// 流程：用供应商补齐连接信息 → 解析密钥（env/明文/本次补充）→ 覆写 s.cfg.LLM →
// rebuildProvider → 写回 local.yaml。
//
// 供三处复用：点「应用」、编辑当前生效模型后保存、以及改供应商的地址/密钥后
// 立即生效 —— 三条路径的语义必须完全一致，否则会出现「改完要重进一次设置」。
func (s *Server) applyModelEntry(m config.ModelEntry, supplied string) error {
	e := s.resolveModel(m)
	key := s.resolveEntryKey(m, supplied)
	if key == "" {
		return fmt.Errorf("该模型没有可用密钥（环境变量未设置，供应商与条目上都没有明文），请在设置中补填")
	}
	llm := &s.cfg.LLM
	llm.Model = m.ID
	llm.DisplayName = m.DisplayName()
	llm.BaseURL = strings.TrimSpace(e.BaseURL)
	llm.Provider = e.Protocol
	llm.APIKey = key
	// 模型条目里设置的「输出上限」（tokens，无单位直填）落到实际请求的 max_tokens；
	// 未设置（0）时保留现有配置值。
	if m.CtxOut > 0 {
		llm.MaxTokens = m.CtxOut
	}
	// 多模态能力声明：整体覆盖，含 nil。
	//
	// ⚠️ 必须无条件赋值 —— 见 config.ResolveModelCredentials 里同名注释：
	// 新条目没声明而不清空，会把上一个模型的能力扣到新模型头上。
	llm.Vision = m.Vision
	llm.Video = m.Video
	if err := s.rebuildProvider(); err != nil {
		return err
	}
	// 运行状态写 state.yaml。这里也是明文密钥的落盘点：阶段 3 把 llm 段缩成
	// 一个 model id 之后，密钥就只留在 providers.yaml / models.yaml 里了。
	if err := s.cfg.SaveState(); err != nil {
		logx.Warnf("模型已切换但写回运行状态失败（重启后需重新应用）: %v", err)
	}
	return nil
}
