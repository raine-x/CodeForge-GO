package plugins

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"codeforge/config"
	"codeforge/pkg/security"
	"codeforge/pkg/tools"
)

// Manager 负责插件的生命周期（加载、健康检查、卸载、重载）与工具注册。
type Manager struct {
	registry *tools.Registry
	policy   *security.Policy
	configs  []config.PluginConfig

	mu      sync.Mutex
	drivers map[string]Driver
	loaded  []string
}

// NewManager 构造插件管理器。
func NewManager(registry *tools.Registry, policy *security.Policy, configs []config.PluginConfig) *Manager {
	return &Manager{
		registry: registry,
		policy:   policy,
		configs:  configs,
		drivers:  map[string]Driver{},
	}
}

// LoadAll 加载全部 enabled 的插件。
func (m *Manager) LoadAll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []string
	for _, cfg := range m.configs {
		if !cfg.Enabled {
			continue
		}
		if err := m.loadLocked(ctx, cfg); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", cfg.Name, err))
			log.Printf("[plugins] 加载插件 %s 失败: %v", cfg.Name, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分插件加载失败: %v", errs)
	}
	return nil
}

// UpdateConfigs 更新插件配置快照（运行期新增/启停后调用，随后 Reload 生效）。
// 不加锁写：与 Reload 串行使用（同一 HTTP 处理流程内）。
func (m *Manager) UpdateConfigs(configs []config.PluginConfig) {
	m.configs = configs
}

// Reload 停止全部插件后重新加载（按最近一次 UpdateConfigs 的配置快照）。
// 仅用于整体一致性重建；单插件增删启停请改用增量方法，避免重启无关 MCP 进程。
func (m *Manager) Reload(ctx context.Context) error {
	m.Stop()
	return m.LoadAll(ctx)
}

// Unload 停止并卸载单个插件（Kill 进程 + 注销其全部工具）。
// 插件不存在时静默返回（删除/停用场景的幂等要求）。
func (m *Manager) Unload(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unloadLocked(name)
}

func (m *Manager) unloadLocked(name string) {
	if d, ok := m.drivers[name]; ok {
		_ = d.Close()
		delete(m.drivers, name)
	}
	kept := m.loaded[:0]
	for _, n := range m.loaded {
		if strings.HasPrefix(n, name+".") {
			m.registry.Unregister(n)
			continue
		}
		kept = append(kept, n)
	}
	m.loaded = kept
}

// LoadOne 加载单个插件（按 name 在配置快照中查找，enabled=false 视为未配置）。
// 已加载时先卸载再重载（幂等）。返回加载失败原因（成功时为空串）。
func (m *Manager) LoadOne(ctx context.Context, name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var cfg *config.PluginConfig
	for i := range m.configs {
		if m.configs[i].Name == name {
			if m.configs[i].Enabled {
				c := m.configs[i]
				cfg = &c
			}
			break
		}
	}
	m.unloadLocked(name) // 幂等：先清旧进程与工具
	if cfg == nil {
		return ""
	}
	if err := m.loadLocked(ctx, *cfg); err != nil {
		return fmt.Sprintf("%s: %v", name, err)
	}
	return ""
}

// Stop 停止并释放全部插件。
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, d := range m.drivers {
		_ = d.Close()
		delete(m.drivers, name)
	}
	for _, n := range m.loaded {
		m.registry.Unregister(n)
	}
	m.loaded = nil
}

// Loaded 返回已加载的插件名列表。
func (m *Manager) Loaded() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.drivers))
	for name := range m.drivers {
		out = append(out, name)
	}
	return out
}

// loadLocked 加载单个插件（调用方需持有锁）。
func (m *Manager) loadLocked(ctx context.Context, cfg config.PluginConfig) error {
	driver, err := buildDriver(cfg)
	if err != nil {
		return err
	}

	remoteTools, err := driver.Tools(ctx)
	if err != nil {
		_ = driver.Close()
		return err
	}

	for _, t := range remoteTools {
		rt, ok := t.(*RemoteTool)
		if !ok {
			continue
		}
		rt.plugin = cfg.Name
		rt.name = qualify(cfg.Name, rt.name)
		m.registry.Register(rt)
		m.loaded = append(m.loaded, rt.name)

		if cfg.SecurityPolicy.RequiresApproval {
			m.policy.RequireApproval(rt.name)
		}
	}

	m.drivers[cfg.Name] = driver
	log.Printf("[plugins] 已加载插件 %s（%s），工具数 %d", cfg.Name, cfg.Type, len(remoteTools))
	return nil
}
