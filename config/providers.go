// providers.go 实现服务端「供应商库」：模型连接信息（Base URL / 协议 / 密钥）
// 的唯一归属地。
//
// 为什么要有这一层
//
//	models.yaml 里每条模型各自保存 base_url 与 key_value。同一个网关下每加一个
//	模型，就要把地址和密钥再抄一遍 —— 网关换域名或密钥轮换时得逐条改，
//	而「同一个 base_url + key 下换一个模型 id」这种最常见的动作也变成了
//	一次完整的表单填写。供应商把这份连接信息收拢成一份，模型条目只留 id 与显示名。
//
// 兼容性（关键）
//
//	模型条目上的 BaseURL / Protocol / KeySource / KeyName / KeyValue 全部降级为
//	「可选覆盖」，留空即继承供应商。因此旧 models.yaml 不需要用户做任何事：
//	启动时 MigrateProviders 会按 (base_url, protocol, key*) 把它们归并成供应商，
//	并把连接信息从条目上搬到供应商，随后原样继续工作。
//
// 文件格式（config/providers.yaml，与 models.yaml 同级、同权限 0600）：
//
//	providers:
//	  - id: p-api-deepseek-com
//	    name: deepseek.com
//	    base_url: https://api.deepseek.com
//	    protocol: openai
//	    key_source: plain
//	    key_name: ""
//	    key_value: sk-...
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Provider 是一个供应商（API 网关 / 官方端点）的连接信息。
//
// 模型条目通过 ProviderID 引用它；引用了供应商的模型，其连接信息一律从
// 供应商解析（除非条目上显式写了覆盖值）。
type Provider struct {
	ID        string `yaml:"id" json:"id"`                 // 唯一键，如 p-api-deepseek-com
	Name      string `yaml:"name" json:"name"`             // 显示名，如 deepseek.com / 上海模型实验室
	BaseURL   string `yaml:"base_url" json:"base_url"`     // 请求地址，该供应商下所有模型共用
	Protocol  string `yaml:"protocol" json:"protocol"`     // openai | anthropic
	KeySource string `yaml:"key_source" json:"key_source"` // env | plain
	KeyName   string `yaml:"key_name" json:"key_name"`     // 环境变量名（KeySource=env 时）
	KeyValue  string `yaml:"key_value" json:"-"`           // 明文 Key，绝不序列化到 JSON
	// Disabled 为真表示该供应商被停用：其下模型不再可选，但配置全部保留。
	// 用「反向布尔」而非 Enabled，是为了让零值（老文件里没有这个字段）等于「启用」。
	Disabled bool `yaml:"disabled,omitempty" json:"disabled"`
}

// DisplayName 返回界面展示名：优先 name，回退 id。
func (p Provider) DisplayName() string {
	if v := strings.TrimSpace(p.Name); v != "" {
		return v
	}
	return p.ID
}

// providersFile 是 providers.yaml 的顶层结构。
type providersFile struct {
	Providers []Provider `yaml:"providers"`
}

// ProviderStore 是服务端内存供应商库，持久化到 config/providers.yaml。
type ProviderStore struct {
	mu      sync.Mutex
	path    string
	entries []Provider
	loaded  bool
}

// NewProviderStore 构造供应商库（path 通常为 <configDir>/providers.yaml）。
//
// path 为空串表示「只在内存中工作」，读写都不落盘 —— 与 ModelStore 同一套刹车：
// 配置目录未知时若拼成相对路径，落盘会写到进程的当前工作目录。
func NewProviderStore(path string) *ProviderStore {
	return &ProviderStore{path: path}
}

// Path 返回落盘路径（空串表示纯内存库）。
func (s *ProviderStore) Path() string { return s.path }

// Load 从磁盘读取供应商库；文件不存在时返回空库（不视为错误）。
func (s *ProviderStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *ProviderStore) loadLocked() error {
	s.entries = nil
	s.loaded = true
	if s.path == "" {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取供应商库 %s 失败: %w", s.path, err)
	}
	// 环境变量展开：YAML 里可写 `${VAR}`，值从进程环境取。
	// 这样密钥不必躺在文件里 —— 文件会被同步盘/备份/打包带走，环境变量不会。
	data, missing := expandEnvYAML(data)
	reportMissingEnvVars("供应商库 "+s.path, missing)

	var f providersFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析供应商库 %s 失败: %w", s.path, err)
	}
	for _, p := range f.Providers {
		if v := strings.TrimSpace(p.ID); v != "" {
			p.ID = v
			p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
			p.Protocol = NormalizeModelProtocol(p.Protocol)
			p.KeySource = NormalizeKeySource(p.KeySource, p.KeyValue)
			s.entries = append(s.entries, p)
		}
	}
	return nil
}

// ensureLoaded 懒加载（文件在进程外被改动后，下一次操作前会重新读取）。
func (s *ProviderStore) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	return s.loadLocked()
}

// Save 把当前供应商库写回磁盘（0600）。
func (s *ProviderStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	f := providersFile{Providers: s.entries}
	data, err := yaml.Marshal(&f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// List 返回全部供应商（副本）。
func (s *ProviderStore) List() []Provider {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureLoaded()
	out := make([]Provider, len(s.entries))
	copy(out, s.entries)
	return out
}

// Find 按 id 查找（大小写不敏感），找不到返回第二返回值 false。
func (s *ProviderStore) Find(id string) (Provider, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.ensureLoaded()
	id = strings.TrimSpace(id)
	if id == "" {
		return Provider{}, false
	}
	for _, p := range s.entries {
		if strings.EqualFold(p.ID, id) {
			return p, true
		}
	}
	return Provider{}, false
}

// Upsert 按 ID 插入或更新（ID 大小写不敏感判重；空 ID 报错）。
func (s *ProviderStore) Upsert(p Provider) error {
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		return fmt.Errorf("供应商 id 不能为空")
	}
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.Protocol = NormalizeModelProtocol(p.Protocol)
	p.KeySource = NormalizeKeySource(p.KeySource, p.KeyValue)
	s.mu.Lock()
	defer s.mu.Unlock()
	// 必须先懒加载：否则首次 Upsert 的修改会被随后的 List() 触发的
	// loadLocked() 当成「磁盘真相」整个清掉。
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	for i := range s.entries {
		if strings.EqualFold(s.entries[i].ID, p.ID) {
			s.entries[i] = p
			return nil
		}
	}
	s.entries = append(s.entries, p)
	return nil
}

// Delete 按 id 删除，返回是否真的删掉了条目。
func (s *ProviderStore) Delete(id string) bool {
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

// HasKey 判断供应商是否有可用密钥（env 命名变量已设置，或明文非空）。
func (s *ProviderStore) HasKey(p Provider) bool {
	return EntryKeySet(ModelEntry{
		KeySource: p.KeySource,
		KeyName:   p.KeyName,
		KeyValue:  p.KeyValue,
	})
}

// Sanitized 返回脱敏视图：明文 key 不下发，只给「是否已配置」。
func (s *ProviderStore) Sanitized() []map[string]any {
	entries := s.List()
	out := make([]map[string]any, len(entries))
	for i, p := range entries {
		out[i] = map[string]any{
			"id":         p.ID,
			"name":       p.Name,
			"base_url":   p.BaseURL,
			"protocol":   p.Protocol,
			"key_source": p.KeySource,
			"key_name":   p.KeyName,
			"key_set":    s.HasKey(p),
			"disabled":   p.Disabled,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 迁移：把「自带连接信息」的旧条目归并成供应商
// ---------------------------------------------------------------------------

// MigrateProviders 把 models.yaml 中自带连接信息（ProviderID 为空）的条目，
// 按 (base_url, protocol, key_source, key_name, key_value) 归并成供应商，
// 并把连接信息从条目上搬到供应商。
//
// 幂等：库里已有任何供应商时直接返回，不做第二次归并 —— 用户手动整理过的
// 供应商分组不会被启动流程覆盖。
//
// 返回值是本次归并的模型条目数。
func MigrateProviders(models *ModelStore, providers *ProviderStore) (int, error) {
	if models == nil || providers == nil {
		return 0, nil
	}
	if len(providers.List()) > 0 {
		return 0, nil
	}
	entries := models.List()
	if len(entries) == 0 {
		return 0, nil
	}

	type group struct {
		first int
		all   []int
	}
	groups := map[string]*group{}
	order := []string{}
	for i, m := range entries {
		if strings.TrimSpace(m.ProviderID) != "" {
			continue
		}
		base := strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
		if base == "" {
			// 没有地址就无从归并（该条目可能依赖官方默认端点），保持自带连接信息。
			continue
		}
		k := strings.Join([]string{
			base,
			NormalizeModelProtocol(m.Protocol),
			NormalizeKeySource(m.KeySource, m.KeyValue),
			strings.TrimSpace(m.KeyName),
			m.KeyValue,
		}, "\x00")
		if _, ok := groups[k]; !ok {
			groups[k] = &group{first: i}
			order = append(order, k)
		}
		groups[k].all = append(groups[k].all, i)
	}
	if len(order) == 0 {
		return 0, nil
	}

	used := map[string]bool{}
	migrated := 0
	for _, k := range order {
		src := entries[groups[k].first]
		name := providerNameFromBase(src.BaseURL)
		p := Provider{
			ID:        uniqueProviderID(name, used),
			Name:      name,
			BaseURL:   strings.TrimRight(strings.TrimSpace(src.BaseURL), "/"),
			Protocol:  NormalizeModelProtocol(src.Protocol),
			KeySource: NormalizeKeySource(src.KeySource, src.KeyValue),
			KeyName:   strings.TrimSpace(src.KeyName),
			KeyValue:  src.KeyValue,
		}
		if err := providers.Upsert(p); err != nil {
			return migrated, err
		}
		for _, i := range groups[k].all {
			e := entries[i]
			e.ProviderID = p.ID
			// 连接信息收归供应商后从条目上清空：留空即「继承」，
			// 这样以后在供应商上换域名 / 轮换密钥，其下模型全部跟着变。
			e.BaseURL = ""
			e.Protocol = ""
			e.KeySource = ""
			e.KeyName = ""
			e.KeyValue = ""
			if err := models.Upsert(e); err != nil {
				return migrated, err
			}
			migrated++
		}
	}
	if err := providers.Save(); err != nil {
		return migrated, err
	}
	if err := models.Save(); err != nil {
		return migrated, err
	}
	return migrated, nil
}

// providerNameFromBase 由请求地址推导供应商默认显示名（取主机名）。
//
// 例：https://api.deepseek.com → deepseek.com；https://open.bigmodel.cn/api/paas/v4
// → open.bigmodel.cn。推导不出来时退回原串，保证永远有个能显示的名字。
func providerNameFromBase(base string) string {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return "未命名供应商"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	host := u.Host
	// 「api.」前缀几乎没有辨识度，去掉后更接近用户对这个网关的称呼。
	if strings.HasPrefix(host, "api.") && len(host) > 4 {
		host = host[4:]
	}
	return host
}

// uniqueProviderID 由显示名生成稳定 id，重名时追加 -2 / -3。
func uniqueProviderID(name string, used map[string]bool) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "provider"
	}
	id := "p-" + slug
	for n := 2; used[id]; n++ {
		id = fmt.Sprintf("p-%s-%d", slug, n)
	}
	used[id] = true
	return id
}

// NormalizeKeySource 归一化密钥来源，并修正自相矛盾的组合：
// 明文值非空却标着 env，只会让密钥静默失效，因此一律判为 plain。
func NormalizeKeySource(source, value string) string {
	if strings.TrimSpace(value) != "" {
		return "plain"
	}
	if strings.EqualFold(strings.TrimSpace(source), "plain") {
		return "plain"
	}
	return "env"
}
