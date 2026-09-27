// Package security 实现动作安全策略引擎与审计日志。
package security

import (
	"regexp"
	"strings"
	"sync"

	"codeforge/config"
)

// Decision 是安全策略的三级判定结果。
type Decision string

const (
	// Allow 自动允许（只读 / 白名单操作）。
	Allow Decision = "allow"
	// Deny 禁止执行（命中黑名单政策）。
	Deny Decision = "deny"
	// Ask 需人工审批（潜在危险动作）。
	Ask Decision = "ask"
)

// Rule 是一条有序匹配的权限规则。Tools / Actions 为空表示匹配任意。
type Rule struct {
	Tools    []string
	Actions  []string
	Decision Decision
}

// builtinDenyPatterns 为内置危险命令黑名单，命中即强制 Deny，优先级最高。
var builtinDenyPatterns = []string{
	`(?i)\brm\s+(-[a-zA-Z]+\s+)*-[a-zA-Z]*(r[a-zA-Z]*f|f[a-zA-Z]*r)[a-zA-Z]*\s+(/\*?|\*)\s*$`,
	`(?i)\bmkfs(\.[a-z0-9]+)?\b`,
	`(?i)\bformat\s+[a-z]:`,
	`(?i)\bdiskpart\b`,
	`(?i)\bshutdown\b`,
	`(?i)\breboot\b`,
	`(?i)\bhalt\b`,
	`(?i)\bdd\s+.*of=/dev/`,
	`(?i)>\s*/dev/sd[a-z]`,
	`(?i):\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`, // fork bomb
	`(?i)\bdel\s+/[fsq]`,
	`(?i)\brmdir\s+/s`,
	`(?i)\bchmod\s+-R\s+777\s+/\s*$`,
	`(?i)\bchown\s+-R\s+.*\s+/\s*$`,
}

// readOnlyTools 为默认只读工具集合。
var readOnlyTools = map[string]bool{
	"read_file":    true,
	"list_dir":     true,
	"search_files": true,
	"git_status":   true,
	"git_log":      true,
}

// 权限模式（前端「权限控制」三态）。
const (
	ModeReadOnly = "readonly" // 只读：只读工具放行，其余一律拒绝
	ModeAsk      = "ask"      // 请求：默认人工审批
	ModeAuto     = "auto"     // 自主：默认放行（黑名单与强制审批仍生效）
)

// Policy 是安全策略引擎。模式可在运行期热切换（Evaluate 并发读安全）。
type Policy struct {
	mu               sync.RWMutex
	mode             string
	defaultDecision  Decision
	autoReadOnly     bool
	askDefault       Decision // ask 模式的原始默认判定（切回时恢复）
	askAutoReadOnly  bool     // ask 模式的原始只读放行开关
	rules            []Rule
	denyPatterns     []*regexp.Regexp
	approvalRequired map[string]bool
}

// NewPolicy 由配置构建策略引擎。
func NewPolicy(cfg config.SecurityConfig) *Policy {
	p := &Policy{
		defaultDecision:  normalizeDecision(cfg.DefaultDecision),
		autoReadOnly:     cfg.AutoApproveReadOnly,
		askDefault:       normalizeDecision(cfg.DefaultDecision),
		askAutoReadOnly:  cfg.AutoApproveReadOnly,
		approvalRequired: map[string]bool{},
	}
	p.SetMode(cfg.PermissionMode)
	for _, r := range cfg.Rules {
		p.rules = append(p.rules, Rule{
			Tools:    r.Tools,
			Actions:  r.Actions,
			Decision: normalizeDecision(r.Decision),
		})
	}
	patterns := append([]string{}, builtinDenyPatterns...)
	patterns = append(patterns, cfg.DenyPatterns...)
	for _, expr := range patterns {
		if re, err := regexp.Compile(expr); err == nil {
			p.denyPatterns = append(p.denyPatterns, re)
		}
	}
	return p
}

// Mode 返回当前权限模式（空值视为 ask）。
func (p *Policy) Mode() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.mode == "" {
		return ModeAsk
	}
	return p.mode
}

// SetMode 热切换权限模式：
//   - readonly：默认 Deny，仅只读工具放行（含声明 IsReadOnly 的工具）；
//     跳过配置规则与插件强制审批，防止 Allow/Ask 规则绕过锁定；
//   - auto：默认 Allow，除黑名单外全部自动通过，不弹任何审批；
//   - ask（默认）：按配置文件的原始 default_decision 与规则执行。
//
// 黑名单在任何模式下都生效。
func (p *Policy) SetMode(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeReadOnly:
		p.mode = ModeReadOnly
		p.defaultDecision = Deny
		p.autoReadOnly = true
	case ModeAuto:
		p.mode = ModeAuto
		p.defaultDecision = Allow
		p.autoReadOnly = true
	default:
		p.mode = ModeAsk
		// 回到配置文件的原始默认，避免上一个模式残留。
		p.defaultDecision = normalizeDecision(string(p.askDefault))
		p.autoReadOnly = p.askAutoReadOnly
	}
}

// Evaluate 对一次工具调用给出判定与理由。
//
// toolName: 工具名；action: 语义动作（如 shell 命令、文件路径）；payload: 原始参数文本。
func (p *Policy) Evaluate(toolName, action, payload string) (Decision, string) {
	subject := strings.TrimSpace(action)
	if subject == "" {
		subject = strings.TrimSpace(payload)
	}

	p.mu.RLock()
	mode := p.mode
	if mode == "" {
		mode = ModeAsk
	}
	autoReadOnly, defaultDecision := p.autoReadOnly, p.defaultDecision
	rules, denyPatterns := p.rules, p.denyPatterns
	approvalRequired := p.approvalRequired
	p.mu.RUnlock()

	// 1) 黑名单强制 Deny（优先级最高，任何模式都不放行）

	for _, re := range denyPatterns {
		if re.MatchString(subject) || re.MatchString(payload) {
			return Deny, "命中危险操作黑名单：" + re.String()
		}
	}

	// 2) 自主模式：除黑名单外全部自动通过 —— 不走规则、不问插件强制审批、
	// 不弹人工审批（越界的放行判定由执行器按模式处理，见 executor.escalate）。
	if mode == ModeAuto {
		// ⚠️ 同 4) 的理由：reason 会进审批弹窗的悬停提示，不能带工具代号。
		return Allow, "自主模式自动放行"
	}

	// 3) 插件声明的强制审批工具
	if approvalRequired[strings.ToLower(toolName)] {
		return Ask, "插件安全策略要求人工审批"
	}

	// 4) 有序规则匹配
	// 只读模式：跳过规则，防止配置里的 Allow/Ask 规则绕过锁定。
	if mode != ModeReadOnly {
		for _, r := range rules {
			if matchAny(r.Tools, toolName) && matchAny(r.Actions, subject) {
				// ⚠️ 这里**刻意不带工具代号**（早先返回 "命中规则：" + toolName，
				// 悬停提示里就出现了「命中规则：write_file」）。
				// 项目自己的纪律是「工具代号是给系统调用的内部名称，界面显示的是中文」
				// （见 agent/prompt.go 与 docs/hitl-approval-phrases.md）。
				// 排查不受影响：AuditEntry 本来就单独记 Tool 字段。
				return r.Decision, "命中安全规则（该操作需要人工确认）"
			}
		}
	}

	// 4) 只读工具在开启自动放行时可提升为 Allow
	if autoReadOnly && readOnlyTools[strings.ToLower(toolName)] {
		return Allow, "只读操作自动放行"
	}

	return defaultDecision, "默认策略"
}

// RequireApproval 将若干工具标记为强制人工审批（供插件安全策略使用）。
func (p *Policy) RequireApproval(toolNames ...string) {
	for _, n := range toolNames {
		if n != "" {
			p.approvalRequired[strings.ToLower(n)] = true
		}
	}
}

// IsReadOnlyTool 判断工具是否属于只读集合。
func IsReadOnlyTool(name string) bool { return readOnlyTools[strings.ToLower(name)] }

func matchAny(patterns []string, subject string) bool {
	if len(patterns) == 0 {
		return true
	}
	lower := strings.ToLower(subject)
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.EqualFold(p, subject) || strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}

func normalizeDecision(s string) Decision {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return Allow
	case "deny":
		return Deny
	default:
		return Ask
	}
}
