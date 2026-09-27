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
// 三个都为 nil 时整段省略，不留下任何 null。
type StateBuiltinPlugins struct {
	SkillCreator *bool `yaml:"skill_creator,omitempty"`
	MultiAgent   *bool `yaml:"multi_agent,omitempty"`
	Plan         *bool `yaml:"plan,omitempty"`
}

// State 是程序自有的运行状态投影。
//
// YAML 键与 Config 一致（字段是其子集），因此 state.yaml 可以直接走
// mergeYAML 合并进 Config —— 读取侧不需要任何新代码。
//
// llm 段目前是整块的（含 api_key）：阶段 3 会把它缩成 `model: <id>`，
// 由模型目录解析出 base_url 与密钥引用，届时明文密钥不再落在这里。
// appearance 整段保留：模糊 0、亮度清空都是可表达的取值，不能省略键。
type State struct {
	LLM        LLMConfig      `yaml:"llm"`
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
		LLM:        c.LLM,
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
		c.BuiltinPlugins.Plan != nil {
		s.BuiltinPlugins = &StateBuiltinPlugins{
			SkillCreator: c.BuiltinPlugins.SkillCreator,
			MultiAgent:   c.BuiltinPlugins.MultiAgent,
			Plan:         c.BuiltinPlugins.Plan,
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
