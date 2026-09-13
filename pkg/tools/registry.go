package tools

import (
	"sort"
	"strings"
	"sync"

	"codeforge/pkg/llm"
)

// Registry 是工具注册中心，提供 Schema 生成与工具路由。
//
// wireNames 维护「下发给 LLM 的名字 → 注册名」的映射：部分上游
// （OpenAI 兼容网关）函数名仅允许 1-64 个 ASCII 字母/数字/下划线/连字符，
// 插件工具名带点号（如 github_tools.create_issue）会被 400 拒绝。
// Definitions() 统一在出关口清洗，ResolveWire() 在调用回来时还原。
type Registry struct {
	mu         sync.RWMutex
	tools      map[string]Tool
	wireNames  map[string]string // wire 名 → 注册名
}

// NewRegistry 创建一个空的工具注册中心。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, wireNames: map[string]string{}}
}

// Register 注册（或覆盖）一个工具。
func (r *Registry) Register(t Tool) {
	if t == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

// Unregister 注销一个工具。
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// Get 按名称获取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回全部工具名（已排序）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// List 返回全部工具。
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// sanitizeName 清洗为上游合法函数名：仅 ASCII 字母/数字/下划线/连字符，
// 其余字符替换为下划线；保留 64 字符上限内（截断在 wireNameFor 保证唯一性时处理）。
func sanitizeName(name string) string {
	var sb strings.Builder
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			sb.WriteRune(c)
		default:
			sb.WriteByte('_') // 点号等 → 下划线（github_tools.create_issue → github_tools_create_issue）
		}
	}
	if sb.Len() > 64 {
		return sb.String()[:64]
	}
	return sb.String()
}

// wireNameFor 为注册名生成合法且唯一的 wire 名。
// 冲突（清洗后撞名）时追加 _2/_3 序号。纯 ASCII 工具名（绝大多数内置工具）
// 清洗后与原名一致，不产生映射开销。
func (r *Registry) wireNameFor(name string, taken map[string]bool) string {
	base := sanitizeName(name)
	wire := base
	for i := 2; taken[wire]; i++ {
		wire = base + "_" + itoa(i)
	}
	taken[wire] = true
	if wire != name {
		r.wireNames[wire] = name // 只有名字变化了才需要映射
	}
	return wire
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + itoa(n%10)
}

// ResolveWire 把 wire 名还原为注册名（模型回调用 wire 名进来）。
// 未映射的（即本来就是合法名）原样返回。
func (r *Registry) ResolveWire(wire string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if real, ok := r.wireNames[wire]; ok {
		return real
	}
	return wire
}

// Definitions 生成供 LLM 使用的工具定义列表（名字统一清洗为上游合法格式）。
func (r *Registry) Definitions() []llm.ToolDef {
	r.mu.Lock() // 写锁：wireNames 是随 Definitions 增量的缓存
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)

	taken := map[string]bool{}
	// 先清掉注销工具遗留的映射，再按当前工具重建
	r.wireNames = map[string]string{}
	defs := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := r.tools[n]
		defs = append(defs, llm.ToolDef{
			Name:        r.wireNameFor(n, taken),
			Description: t.Description(),
			InputSchema: t.InputSchema(),
		})
	}
	r.mu.Unlock()
	return defs
}
