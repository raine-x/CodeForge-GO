// Package config 负责 CodeForge 的 YAML 配置加载、合并与保存。
//
// 加载顺序（后者覆盖前者）：
//
//	default.yaml  ->  local.yaml  ->  环境变量回退
//
// 插件配置独立存放在 plugins.yaml，通过 LoadPlugins 单独加载。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ServerConfig 描述 Web 服务监听参数。
type ServerConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	AutoOpen bool   `yaml:"auto_open"`
}

// LLMConfig 描述大模型接入参数。
type LLMConfig struct {
	Provider    string  `yaml:"provider"` // anthropic | openai（custom 为历史别名，等价 openai）
	BaseURL     string  `yaml:"base_url"`
	APIKey      string  `yaml:"api_key"`
	Model       string  `yaml:"model"`
	DisplayName string  `yaml:"display_name"` // 模型显示名（界面展示用，空则回退 model id）
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`

	// MaxAttempts 是含首次请求在内的总尝试次数（针对上游 429/5xx 等瞬时故障），默认 5。
	MaxAttempts int `yaml:"max_attempts"`
	// RetryBackoffMs 是首次退避间隔（毫秒），之后按 2 倍指数增长，默认 1500。
	RetryBackoffMs int `yaml:"retry_backoff_ms"`
}

// AgentConfig 描述 Agent 引擎行为。
type AgentConfig struct {
	MaxSteps           int `yaml:"max_steps"`
	ContextTokenBudget int `yaml:"context_token_budget"`
	// ContextCompressRatio 是自动压缩的触发比例，缺省 0.95（95%）。
	// 压缩线 = (模型窗口 − 输出预留) × 该比例；
	// 仅在模型窗口已知（模型库条目 ctx_in > 0）时按窗口计算，
	// 窗口未知时改用 ContextTokenBudget 作为绝对阈值。
	ContextCompressRatio float64 `yaml:"context_compress_ratio"`
	SystemPromptFile     string  `yaml:"system_prompt_file"`
	WorkDir              string  `yaml:"work_dir"`
	// HiddenTools 是隐藏工具名列表（Exposure 层的确定性规则）：这些注册工具的
	// 定义不再下发给 LLM（模型收不到定义，通常不会主动调用），但注册/执行/权限/
	// 审计链路不变 —— 隐藏 ≠ 禁止执行，如需隐藏即禁用应在 Executor/Policy 加规则。
	// 按真实工具名匹配（如 "web_fetch"），不是清洗后的 wire 名。
	HiddenTools []string `yaml:"hidden_tools"`
}

// SecurityRule 是一条有序匹配的权限规则。
type SecurityRule struct {
	Tools    []string `yaml:"tools"`
	Actions  []string `yaml:"actions"`
	Decision string   `yaml:"decision"` // allow | deny | ask
}

// SecurityConfig 描述安全策略。
type SecurityConfig struct {
	DefaultDecision     string `yaml:"default_decision"`
	AutoApproveReadOnly bool   `yaml:"auto_approve_readonly"`
	// PermissionMode 是前端「权限控制」的当前模式（readonly/ask/auto），
	// 可通过 /api/perm 热切换并持久化，空值按 ask 处理。
	PermissionMode string `yaml:"permission_mode"`
	// AllowOutsideWorkspace 允许文件 / 命令工具访问工作区之外的路径。
	// 默认 false：解析后的路径必须落在 agent.work_dir 之内，否则直接拒绝。
	AllowOutsideWorkspace bool           `yaml:"allow_outside_workspace"`
	DenyPatterns          []string       `yaml:"deny_patterns"`
	Rules                 []SecurityRule `yaml:"rules"`
}

// PluginSecurityPolicy 描述单个插件的安全策略。
type PluginSecurityPolicy struct {
	RequiresApproval bool     `yaml:"requires_approval"`
	AllowedActions   []string `yaml:"allowed_actions"`
}

// PluginConfig 描述一个插件的加载配置。
type PluginConfig struct {
	Name           string               `yaml:"name"`
	Type           string               `yaml:"type"` // mcp | http | wasm | native
	Enabled        bool                 `yaml:"enabled"`
	Description    string               `yaml:"description"`
	Command        string               `yaml:"command"`
	Args           []string             `yaml:"args"`
	Env            map[string]string    `yaml:"env"`
	Endpoint       string               `yaml:"endpoint"`
	Path           string               `yaml:"path"`
	SecurityPolicy PluginSecurityPolicy `yaml:"security_policy"`
}

// Config 是全局配置聚合。
type Config struct {
	Server    ServerConfig   `yaml:"server"`
	LLM       LLMConfig      `yaml:"llm"`
	Agent     AgentConfig    `yaml:"agent"`
	Security  SecurityConfig `yaml:"security"`
	Notify    NotifyConfig   `yaml:"notify"`
	Subagents SubagentConfig `yaml:"subagents"`
	Web       WebConfig      `yaml:"web"`
	Plugins   []PluginConfig `yaml:"plugins"`
	DataDir   string         `yaml:"data_dir"`
	AuditLog  string         `yaml:"audit_log"`

	// 内置插件开关（增强能力，见 BuiltinPluginsConfig）。
	BuiltinPlugins BuiltinPluginsConfig `yaml:"builtin_plugins"`

	// configDir 记录配置目录，供 Save 使用。
	configDir string `yaml:"-"`
}

// BuiltinPluginsConfig 是内置插件（进程内实现、可开关的增强能力）的开关集。
// 指针字段：缺省（未配置）= 开启，写 false 才关闭。
type BuiltinPluginsConfig struct {
	// SkillCreator：让 AI 在对话中动态创建/迭代工作区技能（SKILL.md）。
	SkillCreator *bool `yaml:"skill_creator"`
	// MultiAgent：允许主智能体委派最多 5 个互不重叠的子智能体。
	MultiAgent *bool `yaml:"multi_agent"`
	// Plan：计划模式（@plan 触发，只读产出结构化项目计划书）。
	Plan *bool `yaml:"plan"`
}

// NotifyConfig 是「任务完成」系统通知（Windows Toast / Linux notify-send）的开关与节流。
//
// 通知只在窗口不可见（切走标签页 / 最小化）时发送，窗口在前台可见时不打扰；
// 两次通知之间还有最小间隔，避免连续多轮任务刷屏。
type NotifyConfig struct {
	// Enabled：指针字段，缺省（未配置）= 开启，写 false 完全关闭系统通知。
	Enabled *bool `yaml:"enabled"`
	// MinIntervalMs 两次通知的最小间隔（毫秒）。<=0 时用默认值 3000。
	MinIntervalMs int `yaml:"min_interval_ms"`
}

// NotifyEnabled 返回系统通知是否启用（缺省 true）。
func (c Config) NotifyEnabled() bool {
	return c.Notify.Enabled == nil || *c.Notify.Enabled
}

// NotifyMinInterval 返回两次通知的最小间隔（缺省 3s）。
func (c Config) NotifyMinInterval() time.Duration {
	if c.Notify.MinIntervalMs <= 0 {
		return 3 * time.Second
	}
	return time.Duration(c.Notify.MinIntervalMs) * time.Millisecond
}

// SkillCreatorEnabled 返回 Skill Creator 是否启用（缺省 true）。
func (c BuiltinPluginsConfig) SkillCreatorEnabled() bool {
	return c.SkillCreator == nil || *c.SkillCreator
}

// MultiAgentEnabled 缺省开启，写 false 才关闭。
func (c BuiltinPluginsConfig) MultiAgentEnabled() bool {
	return c.MultiAgent == nil || *c.MultiAgent
}

// PlanEnabled 缺省开启，写 false 才关闭。
func (c BuiltinPluginsConfig) PlanEnabled() bool {
	return c.Plan == nil || *c.Plan
}

// SubagentConcurrencyCap 是子智能体并发上限的硬上限：配置可以调小，但不能突破它。
// pkg/agent 的 MaxSubagents 直接引用本常量，保证「设置页显示的上限」与
// 「校验时实际生效的上限」永远同源。
const SubagentConcurrencyCap = 5

// SubagentConfig 描述子智能体（多智能体协作）的运行策略，可在设置页热更新。
//
// 总开关不在这里：它与内置插件开关 builtin_plugins.multi_agent 是同一个真源
// （关闭后 delegate_subagents 工具不注册，模型无从调用），设置页读写同一字段，
// 避免出现两个互相矛盾的「开关」。
type SubagentConfig struct {
	// MaxConcurrent 是一次委派允许同时运行的子智能体数量，范围 1..SubagentConcurrencyCap；
	// <=0 或越界时按硬上限处理。
	MaxConcurrent int `yaml:"max_concurrent"`
	// AllowWrite：implement 子智能体是否可写入 / 编辑文件。缺省 true。
	AllowWrite *bool `yaml:"allow_write"`
	// AllowDelete：子智能体是否可删除文件。缺省 true。
	AllowDelete *bool `yaml:"allow_delete"`
	// AllowMemory：子智能体是否可写入用户记忆（save_memory）。缺省 true。
	AllowMemory *bool `yaml:"allow_memory"`
}

// SubagentMaxConcurrent 返回子智能体并发上限（缺省 / 越界时回落硬上限）。
func (c Config) SubagentMaxConcurrent() int {
	n := c.Subagents.MaxConcurrent
	if n <= 0 || n > SubagentConcurrencyCap {
		return SubagentConcurrencyCap
	}
	return n
}

// SubagentAllowWrite 返回子智能体是否可写入文件（缺省 true）。
func (c Config) SubagentAllowWrite() bool { return boolDefault(c.Subagents.AllowWrite, true) }

// SubagentAllowDelete 返回子智能体是否可删除文件（缺省 true）。
func (c Config) SubagentAllowDelete() bool { return boolDefault(c.Subagents.AllowDelete, true) }

// SubagentAllowMemory 返回子智能体是否可写入用户记忆（缺省 true）。
func (c Config) SubagentAllowMemory() bool { return boolDefault(c.Subagents.AllowMemory, true) }

// WebConfig 描述联网工具（web_fetch / web_search）的开关与提供方。
type WebConfig struct {
	// Enabled：联网工具总开关。缺省 false（未配置 API Key 前不打扰模型）。
	Enabled *bool `yaml:"enabled"`
	// SearchProvider：web_search 的提供方，tavily（POST JSON）| searxng（?format=json）。
	// 空 / 未知时 web_search 工具不注册（web_fetch 仍可用）。
	SearchProvider string `yaml:"search_provider"`
	// SearchEndpoint：searxng 实例的 JSON 端点（如 https://searx.example.com/search）。
	SearchEndpoint string `yaml:"search_endpoint"`
	// APIKeyEnv：tavily 的 API Key 环境变量名（缺省 TAVILY_API_KEY）。
	APIKeyEnv string `yaml:"api_key_env"`
	// AllowPrivate：允许访问回环/私网地址（内网服务等）。默认 false ——
	// web_fetch 会拒绝私网目标（SSRF 防护），除非显式放行。
	AllowPrivate bool `yaml:"allow_private"`
}

// WebEnabledAt 返回某 WebConfig 是否启用（缺省 false）。
func (w WebConfig) WebEnabledAt() bool { return w.Enabled != nil && *w.Enabled }

// boolDefault 解析「缺省为真」的三态布尔字段。
func boolDefault(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Default 返回内置默认配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 8420, AutoOpen: true},
		LLM: LLMConfig{
			Provider:       "anthropic",
			BaseURL:        "",
			Model:          "claude-sonnet-4-20250514",
			MaxTokens:      8192,
			Temperature:    0.2,
			MaxAttempts:    5,
			RetryBackoffMs: 1500,
		},
		Agent: AgentConfig{
			MaxSteps:             25,
			ContextTokenBudget:   120000,
			ContextCompressRatio: 0.95,
		},
		Security: SecurityConfig{
			DefaultDecision:     "ask",
			AutoApproveReadOnly: true,
			Rules: []SecurityRule{
				{Tools: []string{"read_file", "list_dir", "search_files"}, Decision: "allow"},
				{Tools: []string{"write_file", "edit_file", "delete_file", "run_command"}, Decision: "ask"},
			},
		},
		DataDir:  ".codeforge",
		AuditLog: ".codeforge/audit.jsonl",
	}
}

// ConfigDir 返回配置目录。
func (c *Config) ConfigDir() string { return c.configDir }

// Load 从 configDir 加载配置：default.yaml 打底，local.yaml 覆盖。
func Load(configDir string) (*Config, error) {
	cfg := Default()
	cfg.configDir = configDir

	if err := mergeYAML(cfg, filepath.Join(configDir, "default.yaml")); err != nil {
		return nil, err
	}
	if err := mergeYAML(cfg, filepath.Join(configDir, "local.yaml")); err != nil {
		return nil, err
	}

	plugins, err := LoadPlugins(filepath.Join(configDir, "plugins.yaml"))
	if err != nil {
		return nil, err
	}
	if len(plugins) > 0 {
		cfg.Plugins = plugins
	}

	applyEnvFallback(cfg)
	normalize(cfg)
	return cfg, nil
}

// LoadPlugins 从 plugins.yaml 加载插件配置；文件不存在时返回空切片。
func LoadPlugins(path string) ([]PluginConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取插件配置失败: %w", err)
	}
	var wrapper struct {
		Plugins []PluginConfig `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, fmt.Errorf("解析插件配置失败: %w", err)
	}
	return wrapper.Plugins, nil
}

// SavePlugins 把插件配置写回 plugins.yaml（0600，与 local.yaml 同级权限）。
func SavePlugins(path string, plugins []PluginConfig) error {
	var wrapper struct {
		Plugins []PluginConfig `yaml:"plugins"`
	}
	wrapper.Plugins = plugins
	data, err := yaml.Marshal(&wrapper)
	if err != nil {
		return fmt.Errorf("序列化插件配置失败: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// Save 将当前配置写入 path（通常为 local.yaml）。
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// mergeYAML 将 YAML 文件合并进 cfg；文件不存在则忽略。
func mergeYAML(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取配置 %s 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	return nil
}

// applyEnvFallback 在 API Key 缺省时尝试从环境变量回退。
func applyEnvFallback(cfg *Config) {
	if cfg.LLM.APIKey != "" {
		return
	}
	candidates := map[string][]string{
		"anthropic": {"CODEFORGE_API_KEY", "ANTHROPIC_API_KEY", "LLM_API_KEY"},
		"openai":    {"CODEFORGE_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY"},
		"custom":    {"CODEFORGE_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY"},
	}
	keys, ok := candidates[strings.ToLower(cfg.LLM.Provider)]
	if !ok {
		keys = []string{"CODEFORGE_API_KEY", "LLM_API_KEY"}
	}
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			cfg.LLM.APIKey = v
			return
		}
	}
}

// normalize 补齐缺省值。
func normalize(cfg *Config) {
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8420
	}
	if cfg.Server.Host == "" {
		cfg.Server.Host = "127.0.0.1"
	}
	if cfg.Agent.MaxSteps <= 0 {
		cfg.Agent.MaxSteps = 25
	}
	if cfg.Agent.ContextTokenBudget <= 0 {
		cfg.Agent.ContextTokenBudget = 120000
	}
	// 压缩比例夹紧到 [0.5, 0.99]：太小会频繁压缩（每次都多一次模型调用），
	// 太大则留给输出与摘要的空间不足。
	if cfg.Agent.ContextCompressRatio < 0.5 || cfg.Agent.ContextCompressRatio > 0.99 {
		cfg.Agent.ContextCompressRatio = 0.95
	}
	if cfg.LLM.MaxTokens <= 0 {
		cfg.LLM.MaxTokens = 8192
	}
	if cfg.DataDir == "" {
		cfg.DataDir = ".codeforge"
	}
	if cfg.AuditLog == "" {
		cfg.AuditLog = filepath.Join(cfg.DataDir, "audit.jsonl")
	}
	if cfg.Security.DefaultDecision == "" {
		cfg.Security.DefaultDecision = "ask"
	}
}

// MaskedAPIKey 返回掩码后的 API Key，供前端展示（绝不返回明文）。
func (c *Config) MaskedAPIKey() string {
	k := c.LLM.APIKey
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", len(k)-8) + k[len(k)-4:]
}

// LocalTemplate 是自动生成的 config/local.yaml 模板内容。
//
// 所有条目均为注释，确保解析时不会覆盖 default.yaml 中的默认值。
const LocalTemplate = `# CodeForge 本地覆盖配置（由程序自动生成，已被 .gitignore 忽略）
#
# 用途：覆盖 config/default.yaml 中的配置项。
# 下列条目默认全部注释；取消注释并按需填写即可生效。
# 也可以直接在 Web 界面「设置」中填写，保存后程序会自动写入本文件。
#
# 提示：API Key 也可以改用环境变量，无需写入本文件：
#   CODEFORGE_API_KEY / ANTHROPIC_API_KEY / OPENAI_API_KEY
#
# 本文件可安全删除：重启程序会自动重建，或由界面「设置」重新生成。

# llm:
#   provider: anthropic          # anthropic | openai
#   base_url: ""                 # 留空使用官方默认地址
#   api_key: "sk-..."            # 敏感信息，切勿提交到版本库
#   model: "claude-sonnet-4-20250514"
#   max_tokens: 8192
#   temperature: 0.2

# server:
#   host: 127.0.0.1
#   port: 8420
#   auto_open: true

# security:
#   default_decision: ask        # allow | deny | ask
#   auto_approve_readonly: true
`

// EnsureLocalTemplate 在 config/local.yaml 缺失时生成一份带注释的模板。
//
// 返回实际路径（configDir 为空时返回空串）。文件权限为 0600，
// 且被 .gitignore 忽略，可安全删除后由程序重建。
func EnsureLocalTemplate(configDir string) (string, error) {
	if configDir == "" {
		return "", nil
	}
	path := filepath.Join(configDir, "local.yaml")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(LocalTemplate), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// LoadDotEnv 依次尝试给定的 .env 路径，读取其中的 KEY=VALUE 并注入进程环境。
//
// 约定：
//   - 支持 `KEY=VALUE`、`export KEY=VALUE`、`#` 注释、单双引号包裹的值；
//   - **已存在于进程环境中的变量不会被覆盖**（真实环境变量优先级更高）；
//   - 所有路径都不存在时静默返回空串，不报错。
//
// 返回实际加载的文件路径（未加载时为 ""）。
func LoadDotEnv(paths ...string) (string, error) {
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("读取 %s 失败: %w", p, err)
		}
		applyDotEnv(string(data))
		return p, nil
	}
	return "", nil
}

// applyDotEnv 解析 .env 内容并设置环境变量。
func applyDotEnv(content string) {
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); exists {
			continue // 环境变量优先
		}
		_ = os.Setenv(key, value)
	}
}
