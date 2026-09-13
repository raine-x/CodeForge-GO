// models.go 实现服务端「模型库」：设置页统一管理的模型条目。
//
// 设计要点（与 local.yaml 的关系）：
//   - local.yaml 只保留「当前生效」的 LLM 配置（单数），由设置页「应用」动作写入；
//   - models.yaml 保存「模型库」（复数），是设置页模型列表的唯一数据源；
//   - 密钥明文只存在服务端这两个被 .gitignore 忽略的文件里，**绝不下发前端**
//     （前端历史实现把明文 key 存进 localStorage，属于安全隐患，已废弃）。
//
// 文件格式（config/models.yaml，与 local.yaml 同级、同权限 0600）：
//
//	models:
//	  - id: z-ai/glm-5.3-free
//	    name: GLM
//	    base_url: https://api.tokenrouter.com/v1
//	    protocol: openai
//	    key_source: env
//	    key_name: CODEFORGE_API_KEY
//	    key_value: ""
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// ModelEntry 是模型库中的一条模型配置。
type ModelEntry struct {
	ID        string `yaml:"id" json:"id"`                 // 模型 id（唯一键），如 z-ai/glm-5.3-free
	Name      string `yaml:"name" json:"name"`             // 显示名，可空（空则回退 ID）
	BaseURL   string `yaml:"base_url" json:"base_url"`     // 请求地址，官方默认端点可留空
	Protocol  string `yaml:"protocol" json:"protocol"`     // openai | anthropic（兼容协议；历史 custom 归一化为 openai）
	KeySource string `yaml:"key_source" json:"key_source"` // env | plain
	KeyName   string `yaml:"key_name" json:"key_name"`     // 环境变量名（KeySource=env 时）
	KeyValue  string `yaml:"key_value" json:"-"`           // 明文 Key（KeySource=plain 时），绝不序列化到 JSON
	CtxIn     int    `yaml:"ctx_in,omitempty" json:"ctx_in,omitempty"`     // 输入上下文上限（tokens，无单位直填），0 表示未设置
	CtxOut    int    `yaml:"ctx_out,omitempty" json:"ctx_out,omitempty"`   // 输出上限（tokens，无单位直填），应用时写入 LLM.MaxTokens
}

// DisplayName 返回界面展示名：优先 name，回退 id。
func (m ModelEntry) DisplayName() string {
	if v := strings.TrimSpace(m.Name); v != "" {
		return v
	}
	return m.ID
}

// modelsFile 是 models.yaml 的顶层结构。
type modelsFile struct {
	Models []ModelEntry `yaml:"models"`
}

// ModelStore 是服务端内存模型库，持久化到 config/models.yaml。
type ModelStore struct {
	mu      sync.Mutex
	path    string
	entries []ModelEntry
	loaded  bool
}

// NewModelStore 构造模型库（path 通常为 <configDir>/models.yaml）。
func NewModelStore(path string) *ModelStore {
	return &ModelStore{path: path}
}

// Load 从磁盘读取模型库；文件不存在时返回空库（不视为错误）。
func (s *ModelStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *ModelStore) loadLocked() error {
	s.entries = nil
	s.loaded = true
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取模型库 %s 失败: %w", s.path, err)
	}
	var f modelsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析模型库 %s 失败: %w", s.path, err)
	}
	for _, m := range f.Models {
		if v := strings.TrimSpace(m.ID); v != "" {
			m.ID = v
			m.Protocol = NormalizeModelProtocol(m.Protocol)
			s.entries = append(s.entries, m)
		}
	}
	return nil
}

// ensureLoaded 懒加载（文件在进程外被改动后，下一次操作前会重新读取）。
func (s *ModelStore) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	return s.loadLocked()
}

// Save 把当前模型库写回磁盘（0600，与 local.yaml 同级权限）。
func (s *ModelStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := modelsFile{Models: s.entries}
	data, err := yaml.Marshal(&f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// List 返回全部模型条目（副本）。
func (s *ModelStore) List() []ModelEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureLoaded()
	out := make([]ModelEntry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Find 按 id 查找（大小写不敏感），找不到返回第二返回值 false。
func (s *ModelStore) Find(id string) (ModelEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureLoaded()
	for _, m := range s.entries {
		if strings.EqualFold(m.ID, strings.TrimSpace(id)) {
			return m, true
		}
	}
	return ModelEntry{}, false
}

// Upsert 按 ID 插入或更新（ID 大小写不敏感判重；空 ID 报错）。
func (s *ModelStore) Upsert(m ModelEntry) error {
	m.ID = strings.TrimSpace(m.ID)
	if m.ID == "" {
		return fmt.Errorf("模型 id 不能为空")
	}
	m.Protocol = NormalizeModelProtocol(m.Protocol)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 必须先懒加载：否则首次 Upsert 的修改会被随后的 List() 触发的
	// loadLocked() 当成「磁盘真相」整个清掉。
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	for i := range s.entries {
		if strings.EqualFold(s.entries[i].ID, m.ID) {
			s.entries[i] = m
			return nil
		}
	}
	s.entries = append(s.entries, m)
	return nil
}

// Delete 按 id 删除，返回是否真的删掉了条目。
func (s *ModelStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLoaded(); err != nil {
		return false
	}
	for i := range s.entries {
		if strings.EqualFold(s.entries[i].ID, strings.TrimSpace(id)) {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			return true
		}
	}
	return false
}

// Sanitized 返回脱敏视图：明文 key 不下发，只给「是否已配置」。
func (s *ModelStore) Sanitized() []map[string]any {
	entries := s.List()
	out := make([]map[string]any, len(entries))
	for i, m := range entries {
		out[i] = map[string]any{
			"id":         m.ID,
			"name":       m.Name,
			"base_url":   m.BaseURL,
			"protocol":   m.Protocol,
			"key_source": m.KeySource,
			"key_name":   m.KeyName,
			"key_set":    s.entryKeySet(m),
		}
	}
	return out
}

// entryKeySet 判断条目是否有可用密钥（env 命名变量已设置，或明文非空）。
func (s *ModelStore) entryKeySet(m ModelEntry) bool {
	if strings.EqualFold(m.KeySource, "env") {
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(m.KeyName))) != ""
	}
	return strings.TrimSpace(m.KeyValue) != ""
}

// NormalizeModelProtocol 把协议名归一化到 openai | anthropic。
// 历史版本用「custom」在界面上区分 OpenAI 兼容网关，但其协议实现与 openai
// 完全相同，现已合并：custom 一律归一化为 openai（旧 models.yaml 重新保存时自动迁移）。
func NormalizeModelProtocol(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "anthropic":
		return "anthropic"
	case "openai", "custom":
		return "openai"
	default:
		return "openai"
	}
}
