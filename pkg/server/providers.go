// providers.go 提供「供应商」的设置页接口。
//
// 供应商是模型连接信息（Base URL / 协议 / 密钥）的唯一归属地。前端选定一个
// 供应商后，新增或切换其下的模型只需要给模型 id —— 地址与密钥由服务端补齐。
//
// 接口：
//
//	GET  /api/providers/list    脱敏列表（明文 key 不下发）
//	POST /api/providers/save    新增 / 更新；改地址或密钥会即时热切换到其下生效模型
//	POST /api/providers/delete  删除；其下模型自动「解绑」并内联连接信息，不会失联
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"codeforge/config"
)

// providerStorePath 返回供应商库落盘路径。
//
// 与 modelStorePath 同一套守卫：配置目录未知（空串）时返回空串，让供应商库
// 退化成纯内存库 —— 否则 filepath.Join("", "providers.yaml") 会把文件写进
// 进程的当前工作目录。
func providerStorePath(cfg *config.Config) string {
	if dir := cfg.ConfigDir(); dir != "" {
		return filepath.Join(dir, "providers.yaml")
	}
	return ""
}

// ProviderStore 返回供应商库（懒加载；供 /api/providers/* 使用）。
func (s *Server) ProviderStore() *config.ProviderStore { return s.providerStore }

// resolveModel 用供应商补齐条目上留空的连接信息，返回「实际会用的那一份」。
func (s *Server) resolveModel(m config.ModelEntry) config.ModelEntry {
	return config.ResolveModel(m, s.providerStore)
}

// providerKey 解析供应商的实际密钥（env 变量 → 明文）。
func providerKey(p config.Provider) string {
	if strings.EqualFold(strings.TrimSpace(p.KeySource), "env") {
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(p.KeyName)))
	}
	return strings.TrimSpace(p.KeyValue)
}

// ensureProvidersMigrated 在服务启动时把旧 models.yaml 归并成供应商。
//
// 只在供应商库为空时生效（见 config.MigrateProviders 的幂等约定），失败不阻断
// 启动：迁移不了顶多维持旧格式，模型照常可用。
func (s *Server) ensureProvidersMigrated() error {
	if s.providerStore == nil || s.modelStore == nil {
		return nil
	}
	if err := s.modelStore.Load(); err != nil {
		return err
	}
	if err := s.providerStore.Load(); err != nil {
		return err
	}
	_, err := config.MigrateProviders(s.modelStore, s.providerStore)
	return err
}

// applyProviderDefaults 用供应商补齐「测试连接 / 获取模型列表」请求里留空的
// 地址、协议与密钥。
//
// 前端选定供应商后不再重复提交这些字段（这正是本次改造要消掉的重复），
// 因此必须在服务端补齐，才能保证「所见即所测」测的就是供应商上那一份配置。
func (s *Server) applyProviderDefaults(req modelTestReq) modelTestReq {
	id := strings.TrimSpace(req.ProviderID)
	if id == "" || s.providerStore == nil {
		return req
	}
	p, ok := s.providerStore.Find(id)
	if !ok {
		return req
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		req.BaseURL = p.BaseURL
	}
	if strings.TrimSpace(req.Protocol) == "" {
		req.Protocol = p.Protocol
	}
	// 密钥同样按「三件套整体继承」：请求里既没有变量名也没有明文时才回退供应商。
	if strings.TrimSpace(req.KeyName) == "" && strings.TrimSpace(req.KeyValue) == "" {
		req.KeySrc = p.KeySource
		req.KeyName = p.KeyName
		req.KeyValue = p.KeyValue
	}
	return req
}

// handleProviderList 返回脱敏后的供应商库与模型库。
//
// 与 /api/models/list 共用同一份载荷（见 modelLibraryPayload）：生效模型及其所属
// 供应商同样带回 key_plain，供应商页的「查看密钥」才有值可显示。
func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, s.modelLibraryPayload())
}

// providerSaveReq 是保存供应商的请求体。不复用 config.Provider 直接解码：
// 其 KeyValue 标了 json:"-"（防止明文 key 序列化下发），保存接口需要接收它。
type providerSaveReq struct {
	config.Provider
	KeyValue string `json:"key_value"` // 明文 Key；编辑时留空 = 保持库中旧值
	// KeyTouched 为真表示用户本次动过密钥输入框（含切换到「明文密钥」）。
	// 为假且明文为空时按「保持库中旧值」处理 —— 前端拿到的是脱敏视图，
	// 明文框为空只说明回显受限，绝不能被当成「用户清空了密钥」。
	KeyTouched bool `json:"key_touched"`
}

// handleProviderSave 新增 / 更新一个供应商（按 id 判重）。
//
// 更新已生效模型的供应商时，会顺带热切换运行配置：在供应商上换域名或轮换密钥，
// 当前对话立刻改用新配置，不需要用户再去点一次「应用」。
func (s *Server) handleProviderSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req providerSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	p := req.Provider
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
	p.KeyValue = strings.TrimSpace(req.KeyValue)
	if p.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "供应商 id 不能为空"})
		return
	}
	if p.Name == "" {
		p.Name = p.ID
	}
	if p.BaseURL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Base URL 不能为空"})
		return
	}

	store := s.ProviderStore()
	old, existed := store.Find(p.ID)
	// 未动过密钥且没给明文 → 沿用库里旧的三件套，避免把密钥覆盖成空。
	if !req.KeyTouched && p.KeyValue == "" {
		if existed {
			p.KeySource = old.KeySource
			p.KeyName = old.KeyName
			p.KeyValue = old.KeyValue
		}
	}
	if err := store.Upsert(p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := store.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存供应商库失败: " + err.Error()})
		return
	}

	// 当前生效模型若挂在这个供应商下，把改动立刻应用到运行配置。
	hot := false
	if m, ok := s.ModelStore().Find(s.cfg.LLM.Model); ok &&
		strings.EqualFold(strings.TrimSpace(m.ProviderID), p.ID) {
		if err := s.applyModelEntry(m, ""); err == nil {
			hot = true
		}
	}

	saved, _ := store.Find(p.ID)
	out := map[string]any{
		"ok":        true,
		"hot":       hot,
		"providers": store.Sanitized(),
		"models":    s.ModelStore().SanitizedResolved(store),
	}
	if v := providerKey(saved); v != "" {
		out["key_plain"] = v // 用户本就拥有该密钥，回填供表单回显/查看
	}
	writeJSON(w, http.StatusOK, out)
}

// handleProviderDelete 删除供应商。
//
// 其下模型不会被一起删掉，也不会失联：删除时把供应商的连接信息**内联**回每条
// 引用它的模型（条目重新变成自带连接信息），因此模型照常可用。
func (s *Server) handleProviderDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Provider string `json:"provider"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	store := s.ProviderStore()
	p, ok := store.Find(body.Provider)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不在列表中: " + body.Provider})
		return
	}
	if !store.Delete(p.ID) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "供应商不在列表中: " + body.Provider})
		return
	}
	detached := s.detachProviderModels(p)
	if err := store.Save(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存供应商库失败: " + err.Error()})
		return
	}
	if len(detached) > 0 {
		if err := s.ModelStore().Save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存模型库失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"detached":  detached,
		"providers": store.Sanitized(),
		"models":    s.ModelStore().SanitizedResolved(store),
	})
}

// detachProviderModels 把 p 的连接信息内联到引用它的每条模型上，并清空 ProviderID。
// 返回被解绑的模型 id。
func (s *Server) detachProviderModels(p config.Provider) []string {
	store := s.ModelStore()
	out := []string{}
	for _, m := range store.List() {
		if !strings.EqualFold(strings.TrimSpace(m.ProviderID), p.ID) {
			continue
		}
		m.ProviderID = ""
		m.BaseURL = p.BaseURL
		m.Protocol = p.Protocol
		m.KeySource = p.KeySource
		m.KeyName = p.KeyName
		m.KeyValue = p.KeyValue
		if err := store.Upsert(m); err != nil {
			continue
		}
		out = append(out, m.ID)
	}
	return out
}
