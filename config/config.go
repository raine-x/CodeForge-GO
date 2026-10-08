// Package config 负责 CodeForge 的 YAML 配置加载、合并与保存。
//
// 加载顺序（后者覆盖前者）：
//
//	default.yaml  ->  local.yaml  ->  ~/.codeforge/state.yaml  ->  环境变量回退
//
// default.yaml 由代码/Git 维护，local.yaml 由用户编辑（程序不写），
// state.yaml 由程序写（见 state.go）。插件目录独立存放在 plugins.yaml。
package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// builtinPluginsYAML 是内建的默认插件配置（与仓库 config/plugins.yaml 同源），
// 通过 go:embed 打进二进制 —— 裸 exe 分发时无需携带 config 目录，
// 也能自带 Parallel Search 等 MCP 服务；用户本地 plugins.yaml 存在时
// 按名称覆盖同名内建条目（启停/改配置）并追加新条目（见 mergePlugins）。
//
//go:embed plugins.yaml
var builtinPluginsYAML []byte

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
	// RetryMode 是重试间隔模式：fixed（每次等同样的间隔）或 backoff（1×→2×→3×→6×
	// 常用退避序列，之后封顶 6×）。空值按 backoff 处理（见 normalize）。
	RetryMode string `yaml:"retry_mode"`
	// RetryBackoffMs 是基础间隔（毫秒）：fixed 模式即每次等待时长；
	// backoff 模式是序列基准（1000ms → 1s/2s/3s/6s），默认 1000。
	RetryBackoffMs int `yaml:"retry_backoff_ms"`

	// Vision / Video 是当前生效模型的**多模态能力声明**，由模型库条目
	// （ModelEntry.Vision / .Video）解析而来 —— 与 provider / base_url /
	// display_name 同属派生值，因此**刻意不进 StateLLM**：存旧副本只会陈旧。
	// 三态语义见 ModelEntry.Vision（nil = 未声明 = 不知道，绝不等于不支持）。
	//
	// agent 的能力门据此在**发请求之前**拦下模型必然拒收的附件；
	// 未声明时仍乐观发送，撞到拒收再记住该模型（见 agent 包的失败记忆）。
	Vision *bool `yaml:"vision,omitempty"`
	Video  *bool `yaml:"video,omitempty"`

	// RPM 是当前生效模型的客户端节流上限（每分钟请求数），0 = 不限制。
	//
	// 与 Vision/Video 不同，**它是标量而不是三态**：0 与「未设置」语义相同
	//（都是不限制），不存在「不知道」与「不支持」的区分 —— 用户没填就是没填，
	// 没有信息可丢。同样不进 StateLLM：由模型条目解析而来，存副本只会陈旧。
	RPM int `yaml:"rpm,omitempty"`
}

// AgentConfig 描述 Agent 引擎行为。
type AgentConfig struct {
	MaxSteps           int `yaml:"max_steps"`
	ContextTokenBudget int `yaml:"context_token_budget"`
	// ContextCompressRatio 是自动压缩的触发比例，缺省 0.80（80%）。
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
	// Name 是插件的**内部标识**：工具注册名是「Name.工具名」，增删改查也都按它定位。
	// 不要拿它当界面文案展示 —— 见 DisplayName。
	Name string `yaml:"name"`
	// DisplayName 是界面显示名（别名），如「并行搜索」而不是 parallel_search。
	// 留空时界面回退到 Name。沿用技能（Skill.DisplayName）与模型（ModelEntry.Name）
	// 的同一套约定：标识与文案分开，改文案不影响工具路由。
	DisplayName    string               `yaml:"display_name"`
	Type           string               `yaml:"type"` // mcp | mcp-http | http | wasm | native
	Enabled        bool                 `yaml:"enabled"`
	Description    string               `yaml:"description"`
	Command        string               `yaml:"command"`
	Args           []string             `yaml:"args"`
	Env            map[string]string    `yaml:"env"`
	Endpoint       string               `yaml:"endpoint"`
	Path           string               `yaml:"path"`
	SecurityPolicy PluginSecurityPolicy `yaml:"security_policy"`
}

// Label 返回该插件的界面显示名：优先别名，没有别名才回退到内部标识。
func (p PluginConfig) Label() string {
	if d := strings.TrimSpace(p.DisplayName); d != "" {
		return d
	}
	return p.Name
}

// Config 是全局配置聚合。
type Config struct {
	Server     ServerConfig   `yaml:"server"`
	LLM        LLMConfig      `yaml:"llm"`
	Agent      AgentConfig    `yaml:"agent"`
	Security   SecurityConfig `yaml:"security"`
	Notify     NotifyConfig   `yaml:"notify"`
	Subagents  SubagentConfig `yaml:"subagents"`
	Web        WebConfig      `yaml:"web"`
	Plugins    []PluginConfig `yaml:"plugins"`
	DataDir    string         `yaml:"data_dir"`
	AuditLog   string         `yaml:"audit_log"`
	Appearance AppearanceConf `yaml:"appearance"`

	// 内置插件开关（增强能力，见 BuiltinPluginsConfig）。
	BuiltinPlugins BuiltinPluginsConfig `yaml:"builtin_plugins"`

	// configDir 记录配置目录，供 Save 使用。
	configDir string `yaml:"-"`
}

// AppearanceConf 是「设置 → 外观 → 新外观」的自定义背景配置。
type AppearanceConf struct {
	// BackgroundPath 是背景图绝对路径（空 = 未设置）。由系统选择器选出的本地文件。
	BackgroundPath string `yaml:"background_path"`
	// BackgroundBlur 是背景模糊值（px，0~40），0 = 不模糊。
	BackgroundBlur int `yaml:"background_blur"`
	// BackgroundBrightness 是背景亮度（%，20~100），100 = 原始亮度。
	BackgroundBrightness int `yaml:"background_brightness"`
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
	// GoalMode：目标模式（@goal_mode 触发，自主验证目标是否达成）。
	GoalMode *bool `yaml:"goal_mode"`
	// VulnerabilityResearch：授权漏洞挖掘（@vuln_hunt / @security_audit 触发）。
	VulnerabilityResearch *bool `yaml:"vulnerability_research"`
	// ReverseAnalysis：本地样本逆向分析（@reverse_analysis 触发）。
	ReverseAnalysis *bool `yaml:"reverse_analysis"`
	// GoalModeMaxRounds：目标模式的自循环轮数上限（一次任务内最多验几次）。
	//
	// 为什么是轮数而不是时长：轮数是**语义**上限 —— 「验 5 次都不过就停下来问人」
	// 这句话对用户是可预期的，而「最多跑 N 分钟」在模型快慢不同的机器上完全不可预期。
	// 单轮耗时由审查者自己的步数预算与工具超时兜底（见 tools.TimeoutPolicy），
	// 不在这里限制。
	GoalModeMaxRounds int `yaml:"goal_mode_max_rounds"`
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

// GoalModeEnabled 缺省开启，写 false 才关闭。
func (c BuiltinPluginsConfig) GoalModeEnabled() bool {
	return c.GoalMode == nil || *c.GoalMode
}

// VulnerabilityResearchEnabled 缺省开启，写 false 才关闭。
func (c BuiltinPluginsConfig) VulnerabilityResearchEnabled() bool {
	return c.VulnerabilityResearch == nil || *c.VulnerabilityResearch
}

// ReverseAnalysisEnabled 缺省开启，写 false 才关闭。
func (c BuiltinPluginsConfig) ReverseAnalysisEnabled() bool {
	return c.ReverseAnalysis == nil || *c.ReverseAnalysis
}

// GoalModeRounds 返回自循环轮数上限，缺省 5。
//
// 上下界都要夹：0/负数会让 goal_verify 第一次就判定「预算耗尽」（功能形同虚设），
// 而一个填进来的 9999 等于没有上限 —— 那是自我循环烧 token 的入口。
func (c BuiltinPluginsConfig) GoalModeRounds() int {
	n := c.GoalModeMaxRounds
	if n <= 0 {
		return GoalModeDefaultRounds
	}
	if n > GoalModeMaxRoundsCap {
		return GoalModeMaxRoundsCap
	}
	return n
}

// 目标模式轮数上下界。
const (
	// GoalModeDefaultRounds 是未配置时的轮数。
	GoalModeDefaultRounds = 5
	// GoalModeMaxRoundsCap 是允许配到的上限。设它是为了给「设置页被填成 9999」
	// 一道硬闸：用户能调，但调不出一个无上限的自我循环。
	GoalModeMaxRoundsCap = 20
)

// SubagentConcurrencyCap 是子智能体并发上限的硬上限：配置可以调小，但不能突破它。
// pkg/agent 的 MaxSubagents 直接引用本常量，保证「设置页显示的上限」与
// 「校验时实际生效的上限」永远同源。
const SubagentConcurrencyCap = 5

// SubagentStepCap 是子智能体单次任务 ReAct 步数的硬上限。
//
// 配置可以调小，也可以不给（0 = 继承主 loop 的 max_steps），但不给上限的话
// 一次委派 5 个子智能体、每个跑满主循环的步数，token 花费会失控。
const SubagentStepCap = 60

// SubagentConfig 描述子智能体（多智能体协作）的运行策略，可在设置页热更新。
//
// 总开关不在这里：它与内置插件开关 builtin_plugins.multi_agent 是同一个真源
// （关闭后 delegate_subagents 工具不注册，模型无从调用），设置页读写同一字段，
// 避免出现两个互相矛盾的「开关」。
type SubagentConfig struct {
	// MaxConcurrent 是一次委派允许同时运行的子智能体数量，范围 1..SubagentConcurrencyCap；
	// <=0 或越界时按硬上限处理。
	MaxConcurrent int `yaml:"max_concurrent"`
	// MaxSteps 是单个子智能体一次任务的 ReAct 步数上限。
	// 0 / 负数 = 继承主 loop 的 agent.max_steps（与主智能体同口径）；
	// 正数 = 单独指定，夹在 1..SubagentStepCap。
	//
	// 为什么不给子智能体一个写死的更小值：探索类任务（读几个文件、搜几处、
	// 交叉验证）经常需要十几步，写死的 8 会在半途硬停，模型被迫交一份
	// 「还没看完」的结论，主智能体再接着做等于把活儿又干了一遍。
	// 需要压成本时在设置页单独调小，而不是靠一个藏在代码里的魔数。
	MaxSteps int `yaml:"max_steps"`
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

// SubagentMaxSteps 返回单个子智能体的步数上限。
//
// 未配置（<=0）时**继承主 loop**的 max_steps —— 与主智能体同口径，
// 不再有一个藏在代码里的写死上限。parent<=0 属配置异常，回落到 8 保证可用。
// 显式配置时夹在 1..SubagentStepCap：越界值按硬上限收敛而不是拒绝保存
// （与 SubagentMaxConcurrent 同一取向，少一个「界面写了却没生效」的坑）。
func (c Config) SubagentMaxSteps(parent int) int {
	n := c.Subagents.MaxSteps
	if n <= 0 {
		if parent <= 0 {
			return 8
		}
		return parent
	}
	if n > SubagentStepCap {
		return SubagentStepCap
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
			MaxSteps:             500,
			ContextTokenBudget:   120000,
			ContextCompressRatio: 0.80,
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
	// 运行状态（~/.codeforge/state.yaml）排在 local.yaml 之后：
	// 它记的是界面上最近一次真实选择，优先级高于用户手写的静态覆盖。
	if err := LoadStateInto(cfg); err != nil {
		return nil, err
	}

	// 插件配置：内建默认（embed 进二进制，裸 exe 自带 Parallel Search 等 MCP
	// 服务）+ 用户本地 plugins.yaml 覆盖同名条目并追加新条目。
	plugins := DefaultPlugins()
	if external, err := LoadPlugins(filepath.Join(configDir, "plugins.yaml")); err != nil {
		return nil, err
	} else if len(external) > 0 {
		plugins = mergePlugins(plugins, external)
	}
	cfg.Plugins = plugins

	applyEnvFallback(cfg)
	normalize(cfg)
	return cfg, nil
}

// DefaultPlugins 返回内建的默认插件配置（解析自内嵌的 plugins.yaml）。
// Parallel Search 默认启用（免费无 Key，充当默认联网来源），其余默认停用。
func DefaultPlugins() []PluginConfig {
	plugins, _ := parsePlugins(builtinPluginsYAML) // 内嵌资源，编译期已定，忽略错误
	return plugins
}

// parsePlugins 解析 plugins.yaml 格式的插件列表（内嵌与磁盘文件共用）。
func parsePlugins(data []byte) ([]PluginConfig, error) {
	var wrapper struct {
		Plugins []PluginConfig `yaml:"plugins"`
	}
	if err := yaml.Unmarshal(data, &wrapper); err != nil {
		return nil, err
	}
	return wrapper.Plugins, nil
}

// mergePlugins 把用户条目合并进内建列表：同名条目以用户为准（可启停/改配置），
// 内建独有的保留，用户新增的追加。名称匹配不区分大小写（与 API 判重一致）。
func mergePlugins(builtin, user []PluginConfig) []PluginConfig {
	out := make([]PluginConfig, 0, len(builtin)+len(user))
	out = append(out, builtin...)
	for _, u := range user {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, u.Name) {
				out[i] = u
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, u)
		}
	}
	return out
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
	plugins, err := parsePlugins(data)
	if err != nil {
		return nil, fmt.Errorf("解析插件配置失败: %w", err)
	}
	return plugins, nil
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

// SaveWholeConfig 将当前配置全量写入 path（仅供测试构造初始配置）。
//
// Deprecated: 生产代码不要调用它 —— 调用它**本身就是 bug**，不是「不推荐」：
//
//  1. 它序列化的是整个 Config（含 Plugins / Rules / DenyPatterns 等切片），
//     而 yaml 对切片是**替换而非合并**。一旦把它写进 default.yaml，
//     后续在 default.yaml 新增的规则会被这里的旧副本静默吃掉。
//     这个坑真实发生过：8 个设置处理器过去各自 cfg.Save(local.yaml)，
//     互相覆盖，security.rules 一度丢过 todo_write 与 web_*。
//  2. 它会把明文密钥整份抄进目标文件。
//
// 写入程序自有字段的正确入口是 [Config.SaveState] —— 它只写 stateProjection
// 刻意选出的子集（见 state.go 的文件头注释）。
//
// 改名而非只加 Deprecated 标注：删掉旧名后，任何生产调用会**编译失败**；
// 标注只是提醒，拦不住人。这比在 CI 里 grep 调用点可靠 —— grep 会漏
// （换别名、换接收者变量名、跨文件包一层）。
func (c *Config) SaveWholeConfig(path string) error {
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
	// 环境变量展开：YAML 里可写 `${VAR}`（如 llm.api_key: ${MY_KEY}）。
	// 这样密钥可以留在环境变量里，配置文件只留引用。
	data, missing := expandEnvYAML(data)
	reportMissingEnvVars("配置 "+path, missing)

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
		cfg.Agent.MaxSteps = 500
	}
	if cfg.Agent.ContextTokenBudget <= 0 {
		cfg.Agent.ContextTokenBudget = 120000
	}
	// 压缩比例夹紧到 [0.5, 0.99]：太小会频繁压缩（每次都多一次模型调用），
	// 太大则留给输出与摘要的空间不足。
	if cfg.Agent.ContextCompressRatio < 0.5 || cfg.Agent.ContextCompressRatio > 0.99 {
		cfg.Agent.ContextCompressRatio = 0.80
	}
	if cfg.LLM.MaxTokens <= 0 {
		cfg.LLM.MaxTokens = 8192
	}
	// 重试间隔模式只认 fixed / backoff，其余一律按 backoff（1→2→3→6 序列）。
	mode := strings.ToLower(strings.TrimSpace(cfg.LLM.RetryMode))
	if mode != "fixed" && mode != "backoff" {
		mode = "backoff"
	}
	cfg.LLM.RetryMode = mode
	// 自定义背景：模糊夹到 0~40，亮度夹到 20~100（未设置时 0 = 不模糊 / 100 = 原亮度，
	// 但 0 亮度会被当成「未配置」，所以亮度默认按 100 归一化）。
	if cfg.Appearance.BackgroundBlur < 0 {
		cfg.Appearance.BackgroundBlur = 0
	}
	if cfg.Appearance.BackgroundBlur > 40 {
		cfg.Appearance.BackgroundBlur = 40
	}
	if cfg.Appearance.BackgroundBrightness <= 0 || cfg.Appearance.BackgroundBrightness > 100 {
		cfg.Appearance.BackgroundBrightness = 100
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
