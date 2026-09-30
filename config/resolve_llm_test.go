package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 写一个 models.yaml / providers.yaml 组合，返回两个 store。
func writeStores(t *testing.T, modelsYAML, providersYAML string) (*ModelStore, *ProviderStore) {
	t.Helper()
	dir := t.TempDir()
	if modelsYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "models.yaml"), []byte(modelsYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if providersYAML != "" {
		if err := os.WriteFile(filepath.Join(dir, "providers.yaml"), []byte(providersYAML), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return NewModelStore(filepath.Join(dir, "models.yaml")), NewProviderStore(filepath.Join(dir, "providers.yaml"))
}

func TestResolveModelCredentials(t *testing.T) {
	t.Run("从模型库取回密钥与端点（state 只存了 id 的场景）", func(t *testing.T) {
		ms, ps := writeStores(t,
			"models:\n  - id: glm-test\n    name: GLM\n    provider_id: p1\n    protocol: openai\n    base_url: https://example.invalid/v1\n    key_value: sk-secret\n    ctx_out: 8192\n",
			"providers: []\n")
		cfg := &Config{}
		cfg.LLM.Model = "glm-test"
		// 模拟「只有模型名」的残缺状态：正是 state.yaml 现在存的东西
		cfg.LLM.APIKey = ""
		cfg.LLM.BaseURL = ""

		ResolveModelCredentials(cfg, ms, ps)
		if cfg.LLM.APIKey != "sk-secret" {
			t.Errorf("应解析出密钥，实际 %q", cfg.LLM.APIKey)
		}
		if cfg.LLM.BaseURL != "https://example.invalid/v1" {
			t.Errorf("应解析出 base_url，实际 %q", cfg.LLM.BaseURL)
		}
		if cfg.LLM.Provider != "openai" {
			t.Errorf("协议应解析为 openai，实际 %q", cfg.LLM.Provider)
		}
		// max_tokens 刻意**不从模型条目覆盖**：它在 applyModelEntry 时已按
		// ctx_out 写入，用户还能手动改 —— 持久化那份才是权威。
		if cfg.LLM.MaxTokens != 0 {
			t.Errorf("解析器不应改 max_tokens（用户的覆盖会白改），实际 %d", cfg.LLM.MaxTokens)
		}
		if cfg.LLM.DisplayName != "GLM" {
			t.Errorf("显示名应取自模型条目，实际 %q", cfg.LLM.DisplayName)
		}
	})

	t.Run("key_source=env → 从环境变量取", func(t *testing.T) {
		t.Setenv("MY_GLM_KEY", "sk-from-env")
		ms, ps := writeStores(t,
			"models:\n  - id: m1\n    key_source: env\n    key_name: MY_GLM_KEY\n", "")
		cfg := &Config{}
		cfg.LLM.Model = "m1"
		ResolveModelCredentials(cfg, ms, ps)
		if cfg.LLM.APIKey != "sk-from-env" {
			t.Errorf("应从环境变量取到密钥，实际 %q", cfg.LLM.APIKey)
		}
	})

	t.Run("⚠️ store 没密钥时**保留**已有的（local.yaml / 环境变量），不能清空", func(t *testing.T) {
		// 这是兼容性的关键：很多用户的密钥从没进过模型库，只在 local.yaml。
		// 那时 store 查不到是正常的，清空就等于「升级即登不上」。
		ms, ps := writeStores(t, "models:\n  - id: m1\n    base_url: https://x.invalid\n", "")
		cfg := &Config{}
		cfg.LLM.Model = "m1"
		cfg.LLM.APIKey = "sk-from-local-yaml"
		ResolveModelCredentials(cfg, ms, ps)
		if cfg.LLM.APIKey != "sk-from-local-yaml" {
			t.Errorf("store 无密钥时必须保留原值，实际 %q（被清空会导致登不上）", cfg.LLM.APIKey)
		}
		if cfg.LLM.BaseURL != "https://x.invalid" {
			t.Errorf("但 store 有的字段仍应覆盖，实际 %q", cfg.LLM.BaseURL)
		}
	})

	t.Run("模型不在库里 → 什么都不动（内联模型名的既有配置）", func(t *testing.T) {
		ms, ps := writeStores(t, "models:\n  - id: other\n", "")
		cfg := &Config{}
		cfg.LLM.Model = "gpt-4o-mini" // 直接写在 local.yaml 里的模型名
		cfg.LLM.BaseURL = "https://api.example.com"
		cfg.LLM.APIKey = "sk-x"
		ResolveModelCredentials(cfg, ms, ps)
		if cfg.LLM.BaseURL != "https://api.example.com" || cfg.LLM.APIKey != "sk-x" {
			t.Errorf("不在模型库时不应改动任何字段，实际 base=%q key=%q", cfg.LLM.BaseURL, cfg.LLM.APIKey)
		}
	})

	t.Run("供应商去重：base_url/key 来自 provider", func(t *testing.T) {
		ms, ps := writeStores(t,
			"models:\n  - id: m1\n    provider_id: p1\n",
			"providers:\n  - id: p1\n    base_url: https://p.invalid/v1\n    protocol: anthropic\n    key_value: sk-prov\n")
		cfg := &Config{}
		cfg.LLM.Model = "m1"
		ResolveModelCredentials(cfg, ms, ps)
		if cfg.LLM.BaseURL != "https://p.invalid/v1" {
			t.Errorf("base_url 应来自供应商，实际 %q", cfg.LLM.BaseURL)
		}
		if cfg.LLM.APIKey != "sk-prov" {
			t.Errorf("密钥应来自供应商，实际 %q", cfg.LLM.APIKey)
		}
		if cfg.LLM.Provider != "anthropic" {
			t.Errorf("协议应来自供应商，实际 %q", cfg.LLM.Provider)
		}
	})

	t.Run("空模型 / nil store 都不 panic", func(t *testing.T) {
		cfg := &Config{}
		if msg := ResolveModelCredentials(cfg, nil, nil); msg == "" {
			t.Errorf("应给出提示信息")
		}
		cfg2 := &Config{}
		cfg2.LLM.Model = "x"
		_ = ResolveModelCredentials(cfg2, nil, nil)
	})
}

// state.yaml 的 llm 段不得再出现 api_key。
func TestStateProjectionOmitsAPIKey(t *testing.T) {
	cfg := &Config{}
	cfg.LLM.Model = "glm-test"
	cfg.LLM.APIKey = "sk-should-never-be-written"
	cfg.LLM.BaseURL = "https://x.invalid"
	cfg.LLM.Provider = "openai"
	// 非零值才该被写出来：投影一律 omitempty，零值不落盘
	// （与 State 其余字段同一原则 —— 状态文件里只留真要说的话）
	cfg.LLM.MaxTokens = 4096
	cfg.LLM.Temperature = 0.25
	cfg.LLM.RetryMode = "fixed"
	cfg.LLM.RetryBackoffMs = 1500

	data, err := yaml.Marshal(cfg.stateProjection())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if strings.Contains(got, "sk-should-never-be-written") {
		t.Errorf("state.yaml 里不得出现明文密钥，实际:\n%s", got)
	}
	if strings.Contains(got, "api_key") {
		t.Errorf("llm 段不应再含 api_key 键，实际:\n%s", got)
	}
	if !strings.Contains(got, "model: glm-test") {
		t.Errorf("应保留 model id，实际:\n%s", got)
	}
	// 派生值不落盘：它们由模型条目决定，存旧副本只会陈旧
	for _, k := range []string{"base_url", "provider"} {
		if strings.Contains(got, k) {
			t.Errorf("llm 段不应再写 %q（派生值，启动时按 model id 解析）", k)
		}
	}
	// 但用户设置必须留：砍掉它们，设置页改完重启就丢
	for _, k := range []string{"max_tokens", "temperature", "retry_mode"} {
		if !strings.Contains(got, k) {
			t.Errorf("llm 段应保留用户可调的 %q（它没有别的持久化位置），实际:\n%s", k, got)
		}
	}
}

// 老 state.yaml 里的明文密钥要被检出（用于打迁移提示），但**不删不改**。
func TestLegacyStateHasPlaintextKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.yaml")

	if has, err := LegacyStateHasPlaintextKey(p); err != nil || has {
		t.Errorf("文件不存在时应返回 (false, nil)，实际 (%v, %v)", has, err)
	}
	if err := os.WriteFile(p, []byte("llm:\n  model: m1\n  api_key: sk-legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	has, err := LegacyStateHasPlaintextKey(p)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !has {
		t.Errorf("应检出老文件里的明文 api_key")
	}
	// 确认它没被这个函数改动 —— 迁移靠「下次保存时不再写」，不是靠删
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "sk-legacy") {
		t.Errorf("函数不应修改原文件")
	}
}

// 老 state.yaml 里的 api_key 仍要能加载进 cfg —— 否则「升级即登不上」。
func TestLegacyPlaintextKeyStillLoads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEFORGE_STATE", filepath.Join(dir, "state.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "state.yaml"),
		[]byte("llm:\n  model: m1\n  api_key: sk-legacy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	if err := LoadStateInto(cfg); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.LLM.Model != "m1" {
		t.Errorf("model 应被 state 覆盖，实际 %q", cfg.LLM.Model)
	}
	if cfg.LLM.APIKey != "sk-legacy" {
		t.Errorf("老文件里的 api_key 仍应生效（兼容期），实际 %q", cfg.LLM.APIKey)
	}
}
