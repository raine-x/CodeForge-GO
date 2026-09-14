package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"codeforge/config"
)

// discoveredModel 是从上游 /models 拉回来的一条模型。
type discoveredModel struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	InLibrary bool   `json:"in_library,omitempty"` // 模型库中是否已有，前端据此标记「已添加」
}

// handleModelDiscover 拉取上游 /models 列表，供「添加模型」时勾选。
//
// 与 /api/models/test 的「所见即所测」不同，这里允许回退：
// 表单没填地址/密钥时，回退到**当前生效**的 LLM 配置（用户常在同一个网关上
// 批量添加模型，没必要重复粘贴）。响应里的 key_from_active 会如实告知前端，
// 避免「用了哪个密钥」不可见。
func (s *Server) handleModelDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req modelTestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}

	proto := config.NormalizeModelProtocol(req.Protocol)
	base := strings.TrimRight(strings.TrimSpace(req.BaseURL), "/")
	if base == "" {
		base = strings.TrimRight(strings.TrimSpace(s.cfg.LLM.BaseURL), "/")
	}
	key := strings.TrimSpace(req.resolveKey())
	keyFromActive := false
	if key == "" {
		if v := strings.TrimSpace(s.cfg.LLM.APIKey); v != "" {
			key, keyFromActive = v, true
		}
	}

	if base == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请求地址不能为空：请填写请求地址，或先在模型库中应用一个模型"})
		return
	}
	if key == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "API Key 为空：请填写密钥或设置环境变量"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second) // 覆盖慢网关
	defer cancel()

	// base_url 约定已含版本段（如 https://host/v1），因此首选 base+/models；
	// 若上游是裸域名再补一层 /v1（部分网关的 /models 不在版本前缀下会 404）。
	paths := []string{"/models"}
	if !strings.HasSuffix(base, "/v1") {
		paths = append(paths, "/v1/models")
	}

	var lastErr string
	for _, p := range paths {
		items, status, err := s.fetchModelList(ctx, base+p, proto, key)
		if err == nil {
			lib := s.ModelStore()
			for i := range items {
				if _, ok := lib.Find(items[i].ID); ok {
					items[i].InLibrary = true
				}
			}
			sort.SliceStable(items, func(i, j int) bool {
				return strings.ToLower(items[i].ID) < strings.ToLower(items[j].ID)
			})
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":              true,
				"models":          items,
				"count":           len(items),
				"endpoint":        base + p,
				"key_from_active": keyFromActive,
			})
			return
		}
		lastErr = err.Error()
		// 只有「路径不对」才换下一个候选；鉴权/限流等错误换路径没有意义。
		if status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": lastErr})
}

// fetchModelList 请求一次 GET <url>/models，返回解析后的模型列表。
// status 用于调用方判断是否值得换个路径重试。
func (s *Server) fetchModelList(ctx context.Context, url, proto, key string) ([]discoveredModel, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("请求构造失败：%v", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if proto == "anthropic" {
		httpReq.Header.Set("x-api-key", key)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, fmt.Errorf("请求超时（30s）：上游响应过慢，请稍后重试或检查网络")
		}
		return nil, 0, fmt.Errorf("请求失败：%v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("%s", modelDiscoverReason(resp.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("读取响应失败：%v", err)
	}
	items := parseModelList(raw)
	if len(items) == 0 {
		return nil, resp.StatusCode, fmt.Errorf("上游返回的模型列表为空或格式无法识别（已尝试解析 data / models 字段）")
	}
	return items, resp.StatusCode, nil
}

// modelDiscoverReason 把上游状态码映射为直白的中文原因。
func modelDiscoverReason(status int) string {
	switch status {
	case 401:
		return "密钥无效或已过期（401）——请检查 API Key"
	case 403:
		return "该密钥无权列出模型（403）——请确认 Key 的权限范围"
	case 404:
		return "模型列表接口不存在（404）——请核对请求地址是否包含正确的版本路径（如 /v1）"
	case 405:
		return "该地址不支持 GET 模型列表（405）"
	case 429:
		return "上游限流（429）——请稍后重试"
	case 500, 502, 503, 504:
		return fmt.Sprintf("上游瞬时故障（HTTP %d）——请稍后重试", status)
	default:
		return fmt.Sprintf("上游返回 HTTP %d", status)
	}
}

// parseModelList 宽容解析各家 /models 响应。
//
// 已知形态：
//
//	OpenAI     {"object":"list","data":[{"id":"gpt-4o"}, ...]}
//	Anthropic  {"data":[{"id":"claude-...","display_name":"Claude"}, ...]}
//	Ollama     {"models":[{"name":"llama3:8b"}, ...]}
//	裸数组     ["gpt-4o", ...]
//
// 网关五花八门，所以这里不按固定结构解析，而是「找数组 → 逐项取 id」，
// 并允许 data/models 再套一层。
func parseModelList(raw []byte) []discoveredModel {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil
	}
	arr := findModelArray(top, 0)
	if arr == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]discoveredModel, 0, len(arr))
	for _, item := range arr {
		m, ok := modelFromAny(item)
		if !ok {
			continue
		}
		lower := strings.ToLower(m.ID)
		if seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, m)
	}
	return out
}

// findModelArray 定位模型数组，最多向下探 4 层，避免在畸形响应里无限递归。
func findModelArray(v any, depth int) []any {
	if depth > 4 {
		return nil
	}
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		for _, k := range []string{"data", "models", "items", "list", "result", "results", "model"} {
			if child, ok := t[k]; ok {
				if arr := findModelArray(child, depth+1); arr != nil {
					return arr
				}
			}
		}
		// 兜底：任意第一个非空数组，兼容没遵循命名的网关
		for _, child := range t {
			if arr, ok := child.([]any); ok && len(arr) > 0 {
				return arr
			}
		}
	}
	return nil
}

// modelFromAny 从数组元素里取出模型 id 与显示名。
//
// id 的候选顺序是 id → name → model → slug：OpenAI 系用 id，
// Ollama 系用 name（此时显示名与 id 相同，前端不重复展示）。
func modelFromAny(v any) (discoveredModel, bool) {
	if s, ok := v.(string); ok {
		id := strings.TrimSpace(s)
		return discoveredModel{ID: id}, id != ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return discoveredModel{}, false
	}
	for _, k := range []string{"id", "name", "model", "slug"} {
		raw, ok := m[k].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		out := discoveredModel{ID: strings.TrimSpace(raw)}
		for _, nk := range []string{"display_name", "displayName", "title"} {
			if n, ok := m[nk].(string); ok && strings.TrimSpace(n) != "" {
				out.Name = strings.TrimSpace(n)
				break
			}
		}
		// 只有当 id 取自非 name 字段时，name 才值得作为显示名带上
		if out.Name == "" && k != "name" {
			if n, ok := m["name"].(string); ok && strings.TrimSpace(n) != "" {
				out.Name = strings.TrimSpace(n)
			}
		}
		return out, true
	}
	return discoveredModel{}, false
}

// modelBatchSaveReq 是「勾选后批量添加」的请求体：
// 每条模型共用同一套连接信息（地址 / 协议 / 密钥），只有 id 与显示名不同。
type modelBatchSaveReq struct {
	Models []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"models"`
	BaseURL   string `json:"base_url"`
	Protocol  string `json:"protocol"`
	KeySource string `json:"key_source"`
	KeyName   string `json:"key_name"`
	KeyValue  string `json:"key_value"`
	CtxIn     int    `json:"ctx_in"`
	CtxOut    int    `json:"ctx_out"`
}

// handleModelSaveBatch 批量添加模型（一次 Upsert + 一次落盘）。
//
// 已存在的条目一律**跳过而不覆盖**：批量添加的语义是「把上游还没有的补进来」，
// 覆盖会悄悄改掉用户手调过的 base_url / 密钥 / 上下文上限。
func (s *Server) handleModelSaveBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req modelBatchSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	if len(req.Models) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请至少选择一个模型"})
		return
	}

	store := s.ModelStore()
	proto := config.NormalizeModelProtocol(req.Protocol)
	keySource := "env"
	if strings.EqualFold(strings.TrimSpace(req.KeySource), "plain") {
		keySource = "plain"
	}

	added := make([]string, 0, len(req.Models))
	skipped := make([]string, 0)
	failed := make([]string, 0)
	for _, item := range req.Models {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, exists := store.Find(id); exists {
			skipped = append(skipped, id)
			continue
		}
		entry := config.ModelEntry{
			ID:        id,
			Name:      strings.TrimSpace(item.Name),
			BaseURL:   strings.TrimSpace(req.BaseURL),
			Protocol:  proto,
			KeySource: keySource,
			KeyName:   strings.TrimSpace(req.KeyName),
			KeyValue:  strings.TrimSpace(req.KeyValue),
			CtxIn:     req.CtxIn,
			CtxOut:    req.CtxOut,
		}
		if err := store.Upsert(entry); err != nil {
			failed = append(failed, id)
			continue
		}
		added = append(added, id)
	}

	if len(added) > 0 {
		if err := store.Save(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "保存模型库失败: " + err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"added":       len(added),
		"skipped":     len(skipped),
		"failed":      len(failed),
		"added_ids":   added,
		"skipped_ids": skipped,
		"failed_ids":  failed,
		"models":      store.Sanitized(),
	})
}
