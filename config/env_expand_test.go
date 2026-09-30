package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandEnvYAMLBasics 基本展开行为。
func TestExpandEnvYAMLBasics(t *testing.T) {
	t.Setenv("CF_EXPAND_URL", "https://api.example.com")
	t.Setenv("CF_EXPAND_KEY", "sk-secret")

	cases := []struct {
		name    string
		in      string
		want    string
		missing []string
	}{
		{"花括号完整替换", "url: ${CF_EXPAND_URL}", "url: https://api.example.com", nil},
		{"裸形式", "url: $CF_EXPAND_URL", "url: https://api.example.com", nil},
		{"嵌在字符串里", "url: ${CF_EXPAND_URL}/v1", "url: https://api.example.com/v1", nil},
		{"同一变量出现两次", "a: ${CF_EXPAND_KEY}\nb: ${CF_EXPAND_KEY}",
			"a: sk-secret\nb: sk-secret", nil},
		{"与普通文本混排", "prefix-${CF_EXPAND_KEY}-suffix",
			"prefix-sk-secret-suffix", nil},
		{"未设置的变量按空处理并报告", "key: ${CF_EXPAND_MISSING}",
			"key: ", []string{"CF_EXPAND_MISSING"}},
		{"美元后不是标识符", "price: $5 and $ 9", "price: $5 and $ 9", nil},
		{"未闭合的花括号当普通文本", "x: ${FOO", "x: ${FOO", nil},
		{"转义后取字面量", `x: \${CF_EXPAND_KEY}`, "x: ${CF_EXPAND_KEY}", nil},
		{"空变量名不当变量", "x: ${}", "x: ${}", nil},
		{"标识符不能以数字开头", "x: ${1BAD}", "x: ${1BAD}", nil},
		{"无美元号走快路径", "a: 1\nb: 2", "a: 1\nb: 2", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_ = os.Unsetenv("CF_EXPAND_MISSING")
			got, missing := expandEnvYAML([]byte(c.in))
			if string(got) != c.want {
				t.Errorf("展开结果\n期望 %q\n实际 %q", c.want, got)
			}
			if len(missing) != len(c.missing) {
				t.Fatalf("未设置变量列表长度 期望 %d(%v) 实际 %d(%v)",
					len(c.missing), c.missing, len(missing), missing)
			}
			for i := range c.missing {
				if missing[i] != c.missing[i] {
					t.Errorf("未设置变量[%d] 期望 %q 实际 %q", i, c.missing[i], missing[i])
				}
			}
		})
	}
}

// TestProviderKeyFromEnvReference 供应商的密钥可以写变量引用。
//
// 这是本项的主用例：文件里不含明文，值从环境变量来。
func TestProviderKeyFromEnvReference(t *testing.T) {
	t.Setenv("CF_TEST_GW_KEY", "sk-from-env")
	t.Setenv("CF_TEST_GW_URL", "https://gw.example.com")

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	body := `
providers:
  - id: gw
    name: 网关
    base_url: ${CF_TEST_GW_URL}/v1
    protocol: openai
    key_source: plain
    key_value: ${CF_TEST_GW_KEY}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProviderStore(path)
	if err := ps.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	got, ok := ps.Find("gw")
	if !ok {
		t.Fatal("取不到供应商 gw")
	}
	if got.KeyValue != "sk-from-env" {
		t.Errorf("密钥应展开为环境变量的值，实际 %q", got.KeyValue)
	}
	if want := "https://gw.example.com/v1"; got.BaseURL != want {
		t.Errorf("base_url 应展开，实际 %q 期望 %q", got.BaseURL, want)
	}
}

// TestModelKeyFromEnvReference 模型条目同样支持变量引用。
func TestModelKeyFromEnvReference(t *testing.T) {
	t.Setenv("CF_TEST_MODEL_KEY", "sk-model-env")

	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	body := `
models:
  - id: fast
    display_name: 快模型
    key_source: plain
    key_value: ${CF_TEST_MODEL_KEY}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := NewModelStore(path)
	if err := ms.Load(); err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	entry, ok := ms.Find("fast")
	if !ok {
		t.Fatal("取不到模型 fast")
	}
	if entry.KeyValue != "sk-model-env" {
		t.Errorf("密钥应展开为环境变量的值，实际 %q", entry.KeyValue)
	}
}

// TestLocalYamlKeyFromEnvReference config/local.yaml 的 llm.api_key 也支持。
func TestLocalYamlKeyFromEnvReference(t *testing.T) {
	t.Setenv("CF_TEST_LOCAL_KEY", "sk-local-env")

	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// default.yaml 必须存在（Load 会先读它）
	def := "llm:\n  provider: \"openai\"\n  api_key: \"\"\n  base_url: \"\"\n  model: \"x\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte(def), 0o600); err != nil {
		t.Fatal(err)
	}
	loc := "llm:\n  api_key: ${CF_TEST_LOCAL_KEY}\n  base_url: ${CF_TEST_LOCAL_URL}\n"
	_ = os.Unsetenv("CF_TEST_LOCAL_URL")
	os.Setenv("CF_TEST_LOCAL_URL", "https://local.example.com")
	if err := os.WriteFile(filepath.Join(cfgDir, "local.yaml"), []byte(loc), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgDir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.LLM.APIKey != "sk-local-env" {
		t.Errorf("llm.api_key 应展开为 %q，实际 %q", "sk-local-env", cfg.LLM.APIKey)
	}
	if cfg.LLM.BaseURL != "https://local.example.com" {
		t.Errorf("llm.base_url 应展开，实际 %q", cfg.LLM.BaseURL)
	}
}

// TestUnsetVarDoesNotBreakLoading 未设置的变量不该让整个配置不可用。
//
// 契约：替换成空 + 报告，而不是解析失败。
// 理由：配了十项错一项时，用户仍应能读到其余九项。
func TestUnsetVarDoesNotBreakLoading(t *testing.T) {
	_ = os.Unsetenv("CF_TEST_NEVER_SET")

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	body := `
providers:
  - id: a
    base_url: https://ok.example.com
  - id: b
    key_value: ${CF_TEST_NEVER_SET}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProviderStore(path)
	if err := ps.Load(); err != nil {
		t.Fatalf("未设置变量不应导致加载失败: %v", err)
	}
	if _, ok := ps.Find("a"); !ok {
		t.Error("供应商 a 应仍可读出")
	}
	b, ok := ps.Find("b")
	if !ok {
		t.Fatal("供应商 b 应仍可读出（只是密钥为空）")
	}
	if b.KeyValue != "" {
		t.Errorf("未设置变量应按空处理，实际 %q", b.KeyValue)
	}
}

// TestExpansionIsNotWriteBack 展开只影响内存，不改文件。
//
// 这条比任何行为断言都直接 —— 它检查的是**文件内容**而不是程序行为。
// 行为断言在展开失败时会给出误导性的通过。
func TestExpansionIsNotWriteBack(t *testing.T) {
	t.Setenv("CF_TEST_WB_KEY", "sk-should-not-persist")

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	body := "providers:\n  - id: x\n    key_value: ${CF_TEST_WB_KEY}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProviderStore(path)
	if err := ps.Load(); err != nil {
		t.Fatal(err)
	}
	// 读文件确认原文未被替换 —— 展开只发生在内存副本上。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(string(raw), "${CF_TEST_WB_KEY}") {
		t.Errorf("文件原文被改写了 —— 展开不应落盘。实际内容：%s", raw)
	}
	if containsStr(string(raw), "sk-should-not-persist") {
		t.Errorf("明文密钥被写进了文件：%s", raw)
	}
}

// TestKeyValueWithVarRefNormalizesToPlain 界面上「明文密钥」那一档填 ${VAR} 时，
// key_source 仍是 plain —— 归一化后仍应是 plain，且值被展开。
//
// 这条覆盖界面那条路径：用户在「明文密钥」框里填 ${MY_KEY}（而不是切到
// 「环境变量」档只填变量名）。两种写法都该能用，展开机制对两者一视同仁。
func TestKeyValueWithVarRefNormalizesToPlain(t *testing.T) {
	t.Setenv("CF_TEST_PLAINREF", "sk-plain-ref")

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")
	body := `
providers:
  - id: p
    key_source: plain
    key_value: ${CF_TEST_PLAINREF}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProviderStore(path)
	if err := ps.Load(); err != nil {
		t.Fatal(err)
	}
	got, ok := ps.Find("p")
	if !ok {
		t.Fatal("取不到供应商 p")
	}
	if got.KeySource != "plain" {
		t.Errorf("key_source 应为 plain，实际 %q", got.KeySource)
	}
	if got.KeyValue != "sk-plain-ref" {
		t.Errorf("key_value 应展开为环境变量的值，实际 %q", got.KeyValue)
	}
	// 且必须能被解析成最终请求用的密钥（不只是字段对）
	if resolveProviderKeyForTest(got) != "sk-plain-ref" {
		t.Errorf("解析出的可用密钥不对")
	}
}

// resolveProviderKeyForTest 复刻 resolveProvider 的取密钥路径，
// 确认展开后的值真的能用（而不只是「字段看起来对」）。
func resolveProviderKeyForTest(p Provider) string {
	if strings.EqualFold(strings.TrimSpace(p.KeySource), "env") {
		return strings.TrimSpace(os.Getenv(strings.TrimSpace(p.KeyName)))
	}
	return strings.TrimSpace(p.KeyValue)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
