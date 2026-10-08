// state.go 实现「程序写的运行状态」与「用户写的配置」的分离。
//
// 为什么要有这个文件：Config.Save() 序列化的是整个 Config，而设置页每次保存都调它，
// 于是 local.yaml 会被写成一份全量转储 —— 插件目录、安全规则、明文密钥全被复制进去，
// 用户的覆盖文件从此和代码里的默认值互相覆盖（详见 docs/修改.md 阶段 2 的取证）。
//
// 分离办法不是搬运数据，而是换写入目标：
//   - local.yaml  只由用户编辑，程序从此不写；
//   - state.yaml  只由程序写，是 local.yaml 的**投影覆盖层**（字段是其子集）。
//
// 投影结构刻意**不含** Plugins / Rules / DenyPatterns 这类切片字段：
// yaml 合并对切片是整体替换，一旦让它们进 state.yaml，就会重新出现
// 「保存一次，默认规则被旧副本截断」的老毛病。
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// stateEnvKey 允许覆盖状态文件位置。测试必须用它，
// 否则每个跑到的用例都会写真实用户的 ~/.codeforge/state.yaml。
const stateEnvKey = "CODEFORGE_STATE"

// StatePath 返回用户级运行状态文件路径（缺省 %USERPROFILE%/.codeforge/state.yaml）。
//
// 与 store.DefaultPath() 同目录：数据与会话在 data.db，运行状态在 state.yaml，
// 两者都是用户级、跨工作区共享。
func StatePath() string {
	if p := os.Getenv(stateEnvKey); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, ".codeforge", "state.yaml")
}

// StateAgent 是 agent 段里归程序写的部分。
//
// work_dir 刻意不加 omitempty：清空工作区是要能落盘的语义，
// 省略键会让 local.yaml 里的旧路径复活（用户删不掉上次的工作区）。
// max_steps 相反 —— 0 不是合法值，normalize 会兜底，省略更安全。
type StateAgent struct {
	WorkDir  string `yaml:"work_dir"`
	MaxSteps int    `yaml:"max_steps,omitempty"`
}

// StateSecurity 只放权限模式：规则、黑名单属于用户/代码维护的策略，不进状态文件。
type StateSecurity struct {
	PermissionMode string `yaml:"permission_mode"`
}

// StateNotify 只放通知开关，节流间隔留给用户。
// nil = 用户没在界面上动过这个开关（沿用缺省开启），故可省略。
type StateNotify struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// StateSubagents 是子智能体策略。三个能力开关用指针 + omitempty，
// 是为了不把「未配置」写成 `allow_write: null` —— 那种噪声正是
// local.yaml 里 multi_agent: null 的来历。
type StateSubagents struct {
	MaxConcurrent int   `yaml:"max_concurrent,omitempty"`
	MaxSteps      int   `yaml:"max_steps,omitempty"`
	AllowWrite    *bool `yaml:"allow_write,omitempty"`
	AllowDelete   *bool `yaml:"allow_delete,omitempty"`
	AllowMemory   *bool `yaml:"allow_memory,omitempty"`
}

// StateBuiltinPlugins 是内置插件开关，同样只用指针 + omitempty：
// 都为 nil 时整段省略，不留下任何 null。
//
// GoalModeMaxRounds 例外，用值类型 —— 0 与「没配」含义不同（0 会被 GoalModeRounds
// 归一成缺省值 5），而 omitempty 会把 0 一起吞掉，那正好是要表达的状态。
type StateBuiltinPlugins struct {
	SkillCreator          *bool `yaml:"skill_creator,omitempty"`
	MultiAgent            *bool `yaml:"multi_agent,omitempty"`
	Plan                  *bool `yaml:"plan,omitempty"`
	GoalMode              *bool `yaml:"goal_mode,omitempty"`
	VulnerabilityResearch *bool `yaml:"vulnerability_research,omitempty"`
	ReverseAnalysis       *bool `yaml:"reverse_analysis,omitempty"`
	GoalModeMaxRounds     int   `yaml:"goal_mode_max_rounds,omitempty"`
}

// StateLLM 是 state.yaml 里 llm 段的内容。
//
// ⚠️ 刻意**不含 api_key / base_url / provider**（2026-09-27，7 阶段重构计划第 3 阶段）。
//
// 早先这一段直接写整个 LLMConfig，于是 **API Key 以明文落进 state.yaml**
// （0600 只是文件权限，不是加密）。而密钥本来就有更合适的家：providers.yaml
// （按供应商去重）或 models.yaml，或环境变量 —— 由 ResolveModelCredentials
// 在启动时按 model id 解析出来。
//
// 下面这些是**用户在设置页调得动、且不是密钥**的项，必须留在这里：
// max_tokens（用户可覆盖模型自带的 ctx_out）、temperature、重试策略。
// 曾经整个 llm 段被砍成只剩 model，结果设置页改的 max_tokens / temperature /
// retry_* 全部在重启后丢失（max_steps_test.go 的 TestConfigMaxStepsOmittedPreservesLimit
// 就是为此红过一次）。
//
// provider / base_url / display_name 属于**派生值**：它们由模型条目决定，
// 存旧副本只会陈旧 —— 用户换了供应商，落盘那份还指着老地址。
//
// 兼容：老的 state.yaml 里若有 `llm.api_key`，仍然会被 LoadStateInto 合进
// cfg.LLM（它 merge 的是整个 Config，不经过本结构），所以**升级不会登不上**；
// 下一次 SaveState 就不再写它了 —— 迁移就是这么自然完成的。
type StateLLM struct {
	// Model 是当前生效的模型 id（对应 models.yaml 里的一条，或内联模型名）。
	Model string `yaml:"model,omitempty"`
	// DisplayName 是界面展示名。空则回退 model id。
	DisplayName string `yaml:"display_name,omitempty"`
	// 以下是用户可调、且非密钥的运行参数。
	MaxTokens      int     `yaml:"max_tokens,omitempty"`
	Temperature    float64 `yaml:"temperature,omitempty"`
	MaxAttempts    int     `yaml:"max_attempts,omitempty"`
	RetryMode      string  `yaml:"retry_mode,omitempty"`
	RetryBackoffMs int     `yaml:"retry_backoff_ms,omitempty"`
}

// State 是程序自有的运行状态投影。
//
// YAML 键与 Config 一致（字段是其子集），因此 state.yaml 可以直接走
// mergeYAML 合并进 Config —— 读取侧不需要任何新代码。
//
// ⚠️ 读取侧走的是**原始 YAML → Config**，不是「反序列化成 State」。
// 所以 State 里字段变窄（比如 llm 段只剩 model）不会影响读取 —— 读到的
// state.yaml 里出现什么键，就覆盖 Config 的对应字段。这正是上面能平滑
// 迁移「老文件里还有 api_key」的原因。
//
// appearance 整段保留：模糊 0、亮度清空都是可表达的取值，不能省略键。
type State struct {
	LLM        StateLLM       `yaml:"llm"`
	Agent      StateAgent     `yaml:"agent"`
	Security   StateSecurity  `yaml:"security"`
	Appearance AppearanceConf `yaml:"appearance"`
	// 指针 + omitempty：yaml 对结构体字段不做空值判定，直接用值类型会写出
	// `notify: {}` 这种空块。没有内容时整段省略，状态文件里只留真要说的话。
	Notify         *StateNotify         `yaml:"notify,omitempty"`
	Subagents      *StateSubagents      `yaml:"subagents,omitempty"`
	BuiltinPlugins *StateBuiltinPlugins `yaml:"builtin_plugins,omitempty"`
}

// stateProjection 从当前配置取出程序自有字段。
//
// 只包含这些字段：plugins / security.rules / deny_patterns 这些用户与代码维护的
// 条目压根不进这个结构，所以也不可能被写回覆盖。
func (c *Config) stateProjection() State {
	s := State{
		// ⚠️ llm 段**不投影 api_key / base_url / provider**（明文密钥不再落盘，
		// 见 StateLLM）。但用户可调、且非密钥的运行参数（max_tokens /
		// temperature / 重试策略）必须留 —— 它们没有别的持久化位置，
		// 砍掉的话设置页改完重启就丢。
		LLM: StateLLM{
			Model:          c.LLM.Model,
			DisplayName:    c.LLM.DisplayName,
			MaxTokens:      c.LLM.MaxTokens,
			Temperature:    c.LLM.Temperature,
			MaxAttempts:    c.LLM.MaxAttempts,
			RetryMode:      c.LLM.RetryMode,
			RetryBackoffMs: c.LLM.RetryBackoffMs,
		},
		Agent:      StateAgent{WorkDir: c.Agent.WorkDir, MaxSteps: c.Agent.MaxSteps},
		Security:   StateSecurity{PermissionMode: c.Security.PermissionMode},
		Appearance: c.Appearance,
	}
	if c.Notify.Enabled != nil {
		s.Notify = &StateNotify{Enabled: c.Notify.Enabled}
	}
	if c.Subagents.MaxConcurrent != 0 || c.Subagents.MaxSteps != 0 ||
		c.Subagents.AllowWrite != nil ||
		c.Subagents.AllowDelete != nil || c.Subagents.AllowMemory != nil {
		s.Subagents = &StateSubagents{
			MaxConcurrent: c.Subagents.MaxConcurrent,
			MaxSteps:      c.Subagents.MaxSteps,
			AllowWrite:    c.Subagents.AllowWrite,
			AllowDelete:   c.Subagents.AllowDelete,
			AllowMemory:   c.Subagents.AllowMemory,
		}
	}
	if c.BuiltinPlugins.SkillCreator != nil || c.BuiltinPlugins.MultiAgent != nil ||
		c.BuiltinPlugins.Plan != nil || c.BuiltinPlugins.GoalMode != nil ||
		c.BuiltinPlugins.VulnerabilityResearch != nil || c.BuiltinPlugins.ReverseAnalysis != nil ||
		c.BuiltinPlugins.GoalModeMaxRounds != 0 {
		s.BuiltinPlugins = &StateBuiltinPlugins{
			SkillCreator:          c.BuiltinPlugins.SkillCreator,
			MultiAgent:            c.BuiltinPlugins.MultiAgent,
			Plan:                  c.BuiltinPlugins.Plan,
			GoalMode:              c.BuiltinPlugins.GoalMode,
			VulnerabilityResearch: c.BuiltinPlugins.VulnerabilityResearch,
			ReverseAnalysis:       c.BuiltinPlugins.ReverseAnalysis,
			GoalModeMaxRounds:     c.BuiltinPlugins.GoalModeMaxRounds,
		}
	}
	return s
}

// SaveState 把程序自有字段写入 state.yaml（0600，密钥可能落在这里）。
//
// 这是运行状态唯一的写盘入口：8 个设置处理器过去各自 `cfg.Save(local.yaml)`，
// 现在都改成调它，local.yaml 从此对程序只读。
func (c *Config) SaveState() error {
	data, err := yaml.Marshal(c.stateProjection())
	if err != nil {
		return fmt.Errorf("序列化运行状态失败: %w", err)
	}
	path := StatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建状态目录失败: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// LoadStateInto 把 state.yaml 合并进 cfg（文件不存在则静默跳过）。
//
// 调用方必须在 local.yaml 之后合并：state.yaml 记的是最近一次真实运行值，
// 优先级高于用户手写的覆盖。
func LoadStateInto(cfg *Config) error {
	return mergeYAML(cfg, StatePath())
}
