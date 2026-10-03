// models.go 实现服务端「模型库」：设置页统一管理的模型条目。
//
// 设计要点（与 local.yaml、providers.yaml 的关系）：
//   - local.yaml 只保留「当前生效」的 LLM 配置（单数），由设置页「应用」动作写入；
//   - models.yaml 保存「模型库」（复数），是设置页模型列表的唯一数据源；
//   - providers.yaml 保存「供应商」（见 providers.go），持有 Base URL / 协议 / 密钥；
//     模型条目通过 provider_id 引用它，连接信息留空即继承。
//   - 密钥明文只存在服务端这几个被 .gitignore 忽略的文件里，**绝不下发前端**
//     （前端历史实现把明文 key 存进 localStorage，属于安全隐患，已废弃）。
//
// 文件格式（config/models.yaml，与 local.yaml 同级、同权限 0600）：
//
//	models:
//	  - id: deepseek-v4-flash-0731
//	    name: DeepSeekV4Flash
//	    provider_id: p-discovery-api-intern-ai-org-cn   # 地址与密钥都在供应商上
//	    ctx_in: 262144
//	    ctx_out: 131072
//	  - id: models/gemini-3.8-flash                      # 声明多模态能力（可省略）
//	    name: Gemini 3.8 Flash
//	    provider_id: p-ai
//	    vision: true        # 收得到图片
//	    video: true         # 收得到视频（原生透传，不是抽帧）
//	    rpm: 60             # 每分钟最多 60 个请求；不写 / 0 = 不限制
//	  - id: gpt-4o                                     # 自带连接信息（旧格式，仍支持）
//	    name: GPT-4o
//	    base_url: https://api.openai.com/v1
//	    protocol: openai
//	    key_source: env
//	    key_name: OPENAI_API_KEY
//
// vision / video 是**三态**：字段不写 = 未声明（= 不知道），写 true / false 才是
// 显式声明。别把它们当普通 bool 用 —— 未声明必须与「不支持」区分开，
// 理由见 ModelEntry.Vision。
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
//
// 自供应商机制引入后，连接信息（BaseURL / Protocol / Key*）全部降级为
// **可选覆盖**：留空即继承 ProviderID 指向的供应商。正常配置里这些字段都是空的，
// 只有「同一个供应商下某个模型需要走不同地址或不同密钥」这种例外才填。
type ModelEntry struct {
	ID         string `yaml:"id" json:"id"`                                       // 模型 id（唯一键），如 z-ai/glm-5.3-free
	Name       string `yaml:"name" json:"name"`                                   // 显示名，可空（空则回退 ID）
	ProviderID string `yaml:"provider_id,omitempty" json:"provider_id,omitempty"` // 归属供应商；空 = 自带连接信息（旧格式，仍完全可用）
	BaseURL    string `yaml:"base_url,omitempty" json:"base_url"`                 // 覆盖：请求地址；留空继承供应商
	Protocol   string `yaml:"protocol,omitempty" json:"protocol"`                 // 覆盖：openai | anthropic；留空继承供应商
	KeySource  string `yaml:"key_source,omitempty" json:"key_source"`             // 覆盖：env | plain；留空继承供应商
	KeyName    string `yaml:"key_name,omitempty" json:"key_name"`                 // 覆盖：环境变量名（KeySource=env 时）
	KeyValue   string `yaml:"key_value,omitempty" json:"-"`                       // 覆盖：明文 Key（KeySource=plain 时），绝不序列化到 JSON
	CtxIn      int    `yaml:"ctx_in,omitempty" json:"ctx_in,omitempty"`           // 输入上下文上限（tokens，无单位直填），0 表示未设置
	CtxOut     int    `yaml:"ctx_out,omitempty" json:"ctx_out,omitempty"`         // 输出上限（tokens，无单位直填），应用时写入 LLM.MaxTokens

	// Vision / Video 声明该模型的多模态能力，**三态**（nil 未声明 / true / false）。
	//
	// 为什么刻意不用裸 bool：models.yaml 由设置页生成、被 .gitignore 忽略，
	// 绝大多数条目**本来就没有**这两个字段。裸 bool 会把「没写」读成 false，
	// 于是每个未声明的模型都被判成「不支持图片」而从此收不到图片 ——
	// 新装用户一条都没声明，图片功能直接全废；已有配置里的 Gemini 条目也会
	// 突然收不到图。**声明缺失必须等于「不知道」，不能等于「不支持」。**
	//
	// 三态各自的用途（见 agent 的能力门）：
	//   nil   未知 → 照旧乐观发送，撞到上游拒收时记住该模型并中止（一次性代价）；
	//   true  显式支持 → 发之前就拦下不支持的附件，不浪费请求；
	//   false 显式不支持 → 附件根本不该进来。
	Vision *bool `yaml:"vision,omitempty" json:"vision,omitempty"` // 支持图片输入
	Video  *bool `yaml:"video,omitempty" json:"video,omitempty"`   // 支持视频输入（原生透传，非抽帧）

	// RPM 是该模型的**客户端节流上限**（每分钟最多几个请求），0 或未设置 = 不限制。
	//
	// 与「上游返回 429」是两件事，别混：
	//   - 429 = 上游嫌我们快（可能是**别人**共用这把密钥挤占了额度），
	//     只能事后等，且默认要等 10 秒（见 pkg/llm 的 postJSON）；
	//   - RPM = 用户自己知道这把密钥每分钟只能发几个，于是**发之前**就错开，
	//     从不撞墙。免费额度 / 共享密钥 / 有明确配额公告的场合才需要设。
	//
	// 为什么不用更常见的「令牌桶」：那允许短时突发，而突发正是撞 429 的原因。
	// 这里用严格滑动窗口（见 pkg/llm/ratelimit.go），宁可慢也不越线。
	RPM int `yaml:"rpm,omitempty" json:"rpm,omitempty"`
}

// SupportsVision 报告该模型是否声明支持图片输入。
//
// ok=false 表示**未声明**（未知）：调用方必须按「乐观发送，失败再记忆」处理，
// 绝不能把未知当成不支持 —— 见 Vision 字段注释里 models.yaml 被 gitignore 的后果。
func (m ModelEntry) SupportsVision() (ok, supported bool) {
	if m.Vision == nil {
		return false, false
	}
	return true, *m.Vision
}

// SupportsVideo 与 SupportsVision 同构，判的是视频输入。
func (m ModelEntry) SupportsVideo() (ok, supported bool) {
	if m.Video == nil {
		return false, false
	}
	return true, *m.Video
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
//
// path 为空串表示「只在内存中工作」：读写都不落盘（见 Load/Save 的守卫）。
// 这是必要的安全刹车 —— 配置目录未知时若把空串拼成相对路径 "models.yaml"，
// 落盘会写到进程的当前工作目录（曾因此在 pkg/server/ 里凭空出现 models.yaml）。
func NewModelStore(path string) *ModelStore {
	return &ModelStore{path: path}
}

// Path 返回落盘路径（空串表示纯内存库）。
func (s *ModelStore) Path() string { return s.path }

// Load 从磁盘读取模型库；文件不存在时返回空库（不视为错误）。
func (s *ModelStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked()
}

func (s *ModelStore) loadLocked() error {
	s.entries = nil
	s.loaded = true
	if s.path == "" { // 纯内存库：无盘可读
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取模型库 %s 失败: %w", s.path, err)
	}
	// 环境变量展开：YAML 里可写 `${VAR}`，值从进程环境取（同 providers.yaml）。
	data, missing := expandEnvYAML(data)
	reportMissingEnvVars("模型库 "+s.path, missing)

	var f modelsFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("解析模型库 %s 失败: %w", s.path, err)
	}
	for _, m := range f.Models {
		if v := strings.TrimSpace(m.ID); v != "" {
			m.ID = v
			m.ProviderID = strings.TrimSpace(m.ProviderID)
			// 协议留空表示「继承供应商」，不能在这里被归一化成 openai ——
			// 否则供应商是 anthropic 时，条目会被自己的空值覆盖掉。
			if strings.TrimSpace(m.Protocol) != "" {
				m.Protocol = NormalizeModelProtocol(m.Protocol)
			}
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
//
// path 为空时直接返回 nil（纯内存库，不落盘）—— 绝不退化成写相对路径 "models.yaml"。
func (s *ModelStore) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
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
	m.ProviderID = strings.TrimSpace(m.ProviderID)
	// 空协议 = 继承供应商，保持空；非空才归一化（custom → openai）。
	if strings.TrimSpace(m.Protocol) != "" {
		m.Protocol = NormalizeModelProtocol(m.Protocol)
	}
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
//
// 输出的是**未解析**的原始字段（base_url 可能为空 = 继承供应商）。设置页要的是
// 补齐后的视图，用 SanitizedResolved。
func (s *ModelStore) Sanitized() []map[string]any {
	return sanitizeEntries(s.List(), nil)
}

// SanitizedResolved 返回脱敏视图，并用供应商把连接信息补齐后再输出。
// 这样前端拿到的 base_url / protocol / key_set 就是「实际会用的那一份」，
// 不必自己再拼一遍继承逻辑。
func (s *ModelStore) SanitizedResolved(ps *ProviderStore) []map[string]any {
	return sanitizeEntries(s.List(), ps)
}

func sanitizeEntries(entries []ModelEntry, ps *ProviderStore) []map[string]any {
	out := make([]map[string]any, len(entries))
	for i, raw := range entries {
		m := ResolveModel(raw, ps)
		// 供应商**显示名**：主界面模型弹层要按「供应商 ↓ 显示名称」列出，
		// 只给 provider_id 的话前端还得自己再拉一份供应商表来拼。
		// ps 为 nil（未解析视图）时留空，前端回退到只显示名称。
		provName := ""
		if ps != nil && strings.TrimSpace(raw.ProviderID) != "" {
			if p, ok := ps.Find(raw.ProviderID); ok {
				provName = p.Name
				if strings.TrimSpace(provName) == "" {
					provName = p.ID
				}
			}
		}
		out[i] = map[string]any{
			"id":            m.ID,
			"name":          m.Name,
			"provider_id":   raw.ProviderID,
			"provider_name": provName,
			"base_url":      m.BaseURL,
			"protocol":      m.Protocol,
			"key_source":    m.KeySource,
			"key_name":      m.KeyName,
			"key_set":       EntryKeySet(m),
			// 上下文上限必须下发：设置页表单要回显、上下文进度条要展示模型窗口。
			// 早前漏了这两项，表单只能落到前端默认值（262144），用户改过也看不到。
			"ctx_in":  m.CtxIn,
			"ctx_out": m.CtxOut,
			// 多模态能力**按三态下发**：未声明是 null（不是 false）。
			// 前端据此把开关留在「未设置」而不是替用户猜一个值 ——
			// 猜错的后果是模型永久收不到图，而用户没有任何入口去纠正它。
			"vision": raw.Vision,
			"video":  raw.Video,
			// rpm 必须下发：设置页表单要回显，且「0 = 不限制」这个语义
			// 只能靠真值传达（不写该字段会被读成「未知」）。
			"rpm": raw.RPM,
		}
	}
	return out
}

// EntryKeySet 判断一条（已补齐供应商信息的）条目是否有可用密钥：
// env 命名的变量已设置，或明文非空。
func EntryKeySet(m ModelEntry) bool {
	if strings.EqualFold(strings.TrimSpace(m.KeySource), "env") {
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(m.KeyName))) != ""
	}
	return strings.TrimSpace(m.KeyValue) != ""
}

// ResolveModel 用供应商补齐条目上留空的连接信息，返回「实际会用的那一份」。
//
// 找不到供应商（条目自带连接信息，或供应商已被删）时原样返回，只是把空协议
// 兜底成 openai —— 旧格式条目因此完全不受供应商机制影响。
func ResolveModel(m ModelEntry, ps *ProviderStore) ModelEntry {
	if strings.TrimSpace(m.Protocol) != "" {
		m.Protocol = NormalizeModelProtocol(m.Protocol)
	}
	id := strings.TrimSpace(m.ProviderID)
	if ps == nil || id == "" {
		if m.Protocol == "" {
			m.Protocol = "openai"
		}
		return m
	}
	p, ok := ps.Find(id)
	if !ok {
		if m.Protocol == "" {
			m.Protocol = "openai"
		}
		return m
	}
	return m.MergeProvider(p)
}

// MergeProvider 返回把供应商连接信息补齐后的副本：条目上的非空字段优先，
// 因此单条模型仍可以覆盖地址或协议（例外场景），但默认什么都不用填。
//
// 密钥按「三件套整体继承」处理：条目只要没有自己的 KeyName 与 KeyValue，
// 就整套（来源 / 变量名 / 明文）取自供应商 —— 逐字段继承会拼出
// 「来源=plain、变量名=别人的 env 名、值=空」这种自相矛盾的组合。
func (m ModelEntry) MergeProvider(p Provider) ModelEntry {
	out := m
	if strings.TrimSpace(out.BaseURL) == "" {
		out.BaseURL = p.BaseURL
	}
	if strings.TrimSpace(out.Protocol) == "" {
		out.Protocol = p.Protocol
	}
	if strings.TrimSpace(out.KeyName) == "" && strings.TrimSpace(out.KeyValue) == "" {
		out.KeySource = p.KeySource
		out.KeyName = p.KeyName
		out.KeyValue = p.KeyValue
	}
	out.Protocol = NormalizeModelProtocol(out.Protocol)
	return out
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
