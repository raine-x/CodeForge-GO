package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureLocalTemplate(t *testing.T) {
	dir := t.TempDir()

	path, err := EnsureLocalTemplate(dir)
	if err != nil {
		t.Fatalf("生成模板失败: %v", err)
	}
	if path == "" {
		t.Fatal("期望返回模板路径")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("模板文件未生成: %v", err)
	}

	// 幂等：再次调用不应报错，也不应改动已有文件
	if _, err := EnsureLocalTemplate(dir); err != nil {
		t.Fatalf("重复调用失败: %v", err)
	}

	// 模板全为注释，加载后默认值必须保持不变
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("加载含模板的配置失败: %v", err)
	}
	def := Default()
	if cfg.LLM.Model != def.LLM.Model {
		t.Errorf("模板不应覆盖默认 model: %q != %q", cfg.LLM.Model, def.LLM.Model)
	}
	if cfg.Server.Port != def.Server.Port {
		t.Errorf("模板不应覆盖默认 port: %d != %d", cfg.Server.Port, def.Server.Port)
	}
	if cfg.Agent.MaxSteps != def.Agent.MaxSteps {
		t.Errorf("模板不应覆盖默认 max_steps: %d != %d", cfg.Agent.MaxSteps, def.Agent.MaxSteps)
	}
}

func TestMaskedAPIKey(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"", ""},
		{"short", "*****"},
		{"sk-1234567890abcdef", "sk-1***********cdef"},
	}
	for _, c := range cases {
		cfg := &Config{LLM: LLMConfig{APIKey: c.key}}
		if got := cfg.MaskedAPIKey(); got != c.want {
			t.Errorf("MaskedAPIKey(%q) = %q，期望 %q", c.key, got, c.want)
		}
	}
}

// TestDefaultPluginsBuiltin 验证内嵌的默认插件随二进制自带：
// 裸 exe 分发时无需 config 目录，Parallel Search 也应默认启用。
func TestDefaultPluginsBuiltin(t *testing.T) {
	plugins := DefaultPlugins()
	if len(plugins) == 0 {
		t.Fatal("内建插件列表为空 —— go:embed 的 plugins.yaml 未生效")
	}
	var parallel *PluginConfig
	for i := range plugins {
		if plugins[i].Name == "parallel_search" {
			parallel = &plugins[i]
		}
	}
	if parallel == nil {
		t.Fatalf("内建列表缺少 parallel_search，实际: %v", pluginNames(plugins))
	}
	if !parallel.Enabled {
		t.Error("parallel_search 应默认启用（充当默认联网来源）")
	}
	if parallel.Type != "mcp-http" || parallel.Endpoint != "https://search.parallel.ai/mcp" {
		t.Errorf("parallel_search 配置不对: type=%q endpoint=%q", parallel.Type, parallel.Endpoint)
	}
	// 内建插件必须带中文别名：用户反馈「显示代号不好看」——
	// 界面上要看到「并行搜索」而不是 parallel_search。
	if parallel.Label() != "并行搜索" {
		t.Errorf("parallel_search 的显示名期望「并行搜索」，实际 %q", parallel.Label())
	}
	for _, p := range plugins {
		if strings.TrimSpace(p.DisplayName) == "" {
			t.Errorf("内建插件 %q 缺 display_name（界面会露出英文代号）", p.Name)
		}
	}
}

// Label 优先别名、没有别名才回退到内部标识。
func TestPluginLabelFallsBackToName(t *testing.T) {
	for _, c := range []struct{ display, name, want string }{
		{"并行搜索", "parallel_search", "并行搜索"},
		{"", "my_tool", "my_tool"},
		{"   ", "my_tool", "my_tool"},
		{"  并行搜索  ", "parallel_search", "并行搜索"},
	} {
		got := PluginConfig{Name: c.name, DisplayName: c.display}.Label()
		if got != c.want {
			t.Errorf("Label(display=%q, name=%q) 期望 %q，实际 %q", c.display, c.name, c.want, got)
		}
	}
}

// TestLoadMergesBuiltinPlugins 验证本地 plugins.yaml 与内建列表的合并语义：
// 同名以用户为准（可停用内建、改端点），内建保留，用户新增追加。
func TestLoadMergesBuiltinPlugins(t *testing.T) {
	dir := t.TempDir()
	user := "plugins:\n" +
		"  - name: parallel_search\n" +
		"    type: mcp-http\n" +
		"    enabled: false\n" + // 停用内建
		"    endpoint: https://search.parallel.ai/mcp\n" +
		"  - name: my_tool\n" + // 追加新插件
		"    type: mcp\n" +
		"    enabled: true\n" +
		"    command: my-server\n"
	if err := os.WriteFile(filepath.Join(dir, "plugins.yaml"), []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	byName := map[string]PluginConfig{}
	for _, p := range cfg.Plugins {
		byName[p.Name] = p
	}
	if p, ok := byName["parallel_search"]; !ok {
		t.Fatal("合并后应保留内建的 parallel_search")
	} else if p.Enabled {
		t.Error("用户的 enabled:false 应覆盖内建的默认启用")
	}
	if p, ok := byName["my_tool"]; !ok {
		t.Fatal("用户新增的 my_tool 应被追加")
	} else if !p.Enabled || p.Command != "my-server" {
		t.Errorf("my_tool 合并不对: %+v", p)
	}
	if _, ok := byName["exa_search"]; !ok {
		t.Error("未被子目录覆盖的内建条目（exa_search）应保留")
	}
}

// TestLoadWithoutPluginsYamlUsesBuiltin 裸 exe 场景：配置目录没有 plugins.yaml，
// 插件列表应完整来自内嵌默认（不报错、不缺 Parallel）。
func TestLoadWithoutPluginsYamlUsesBuiltin(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("无 plugins.yaml 时 Load 不应报错: %v", err)
	}
	if len(cfg.Plugins) != len(DefaultPlugins()) {
		t.Errorf("无本地文件时应使用全部内建插件，实际 %d 条，期望 %d 条",
			len(cfg.Plugins), len(DefaultPlugins()))
	}
}

func pluginNames(ps []PluginConfig) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// TestAPIKeyComesFromSystemEnvOnly 密钥只从**系统环境变量**读。
//
// 这条钉住「不读密钥文件」这个契约：优先级链是
// CODEFORGE_API_KEY → 厂商专用名 → LLM_API_KEY；
// 而 local.yaml 里写了 api_key 时**优先于**环境变量（那是用户显式配置，
// 不该被环境悄悄盖掉）。
func TestAPIKeyComesFromSystemEnvOnly(t *testing.T) {
	t.Run("系统环境变量可解析出密钥", func(t *testing.T) {
		t.Setenv("CODEFORGE_API_KEY", "from-system-env")
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "from-system-env" {
			t.Errorf("应取到 CODEFORGE_API_KEY，实际 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("厂商专用名次之", func(t *testing.T) {
		t.Setenv("ANTHROPIC_API_KEY", "anthropic-key")
		cfg := &Config{}
		cfg.LLM.Provider = "anthropic"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "anthropic-key" {
			t.Errorf("应取到 ANTHROPIC_API_KEY，实际 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("LLM_API_KEY 是最后一档", func(t *testing.T) {
		t.Setenv("LLM_API_KEY", "generic-key")
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "generic-key" {
			t.Errorf("应回退到 LLM_API_KEY，实际 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("CODEFORGE_API_KEY 优先级最高", func(t *testing.T) {
		t.Setenv("CODEFORGE_API_KEY", "top")
		t.Setenv("OPENAI_API_KEY", "second")
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "top" {
			t.Errorf("CODEFORGE_API_KEY 应覆盖 OPENAI_API_KEY，实际 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("配置里已有值时不被环境覆盖", func(t *testing.T) {
		t.Setenv("CODEFORGE_API_KEY", "from-env")
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		cfg.LLM.APIKey = "explicit-in-config"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "explicit-in-config" {
			t.Errorf("配置里的显式值应保留，实际被改成 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("没有任何来源时保持为空", func(t *testing.T) {
		for _, k := range []string{"CODEFORGE_API_KEY", "OPENAI_API_KEY", "LLM_API_KEY"} {
			_ = os.Unsetenv(k)
		}
		cfg := &Config{}
		cfg.LLM.Provider = "openai"
		applyEnvFallback(cfg)
		if cfg.LLM.APIKey != "" {
			t.Errorf("无任何来源时应为空，实际 %q", cfg.LLM.APIKey)
		}
	})
}

// TestNoDotEnvLoading 确认代码里已无「读密钥文件」的加载路径。
//
// 这条是**结构性**护栏：只看行为测不出来
// （加载一个不存在的文件本来就静默成功），所以直接查源码。
func TestNoDotEnvLoading(t *testing.T) {
	b, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("读取 config.go 失败: %v", err)
	}
	src := string(b)
	for _, banned := range []string{"LoadDotEnv", "applyDotEnv"} {
		if strings.Contains(src, banned) {
			t.Errorf("config.go 里仍有 %s —— 密钥只应从系统环境变量读", banned)
		}
	}
}

// TestSaveWorkDirRoundTrip 验证 agent.work_dir 写入 local.yaml 后能被 Load 恢复
// （工作区持久化的配置层保证）。
func TestSaveWorkDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte(
		"server:/n  port: 8420\nagent:/n  work_dir: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := `C:/Users/test/ws` + string(filepath.Separator)
	cfg.Agent.WorkDir = want
	if err := cfg.SaveWholeConfig(filepath.Join(cfgDir, "local.yaml")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	cfg2, err := Load(cfgDir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if cfg2.Agent.WorkDir != want {
		t.Fatalf("work_dir 回读不符: got %q want %q", cfg2.Agent.WorkDir, want)
	}
}
