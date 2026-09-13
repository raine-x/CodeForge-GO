package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnvParsesForms(t *testing.T) {
	keys := []string{
		"CODEFORGE_TEST_A", "CODEFORGE_TEST_B", "CODEFORGE_TEST_C", "CODEFORGE_TEST_D",
	}
	unsetAll(keys)
	t.Cleanup(func() { unsetAll(keys) })

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := `# 这是注释
CODEFORGE_TEST_A=plain
export CODEFORGE_TEST_B="quoted value"
CODEFORGE_TEST_C='single quoted'
CODEFORGE_TEST_D=
这不是一条赋值语句
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 .env 失败: %v", err)
	}

	got, err := LoadDotEnv(path)
	if err != nil {
		t.Fatalf("LoadDotEnv 报错: %v", err)
	}
	if got != path {
		t.Errorf("返回路径期望 %q，实际 %q", path, got)
	}
	if v := os.Getenv("CODEFORGE_TEST_A"); v != "plain" {
		t.Errorf("普通赋值解析错误: %q", v)
	}
	if v := os.Getenv("CODEFORGE_TEST_B"); v != "quoted value" {
		t.Errorf("export + 双引号解析错误: %q", v)
	}
	if v := os.Getenv("CODEFORGE_TEST_C"); v != "single quoted" {
		t.Errorf("单引号解析错误: %q", v)
	}
	if _, ok := os.LookupEnv("CODEFORGE_TEST_D"); !ok {
		t.Error("空值变量也应被注入（存在即为已设置）")
	}
}

func TestLoadDotEnvDoesNotOverrideRealEnv(t *testing.T) {
	t.Setenv("CODEFORGE_TEST_KEEP", "from-env")

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("CODEFORGE_TEST_KEEP=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadDotEnv(path); err != nil {
		t.Fatalf("LoadDotEnv 报错: %v", err)
	}
	if v := os.Getenv("CODEFORGE_TEST_KEEP"); v != "from-env" {
		t.Errorf("真实环境变量优先级更高，期望 from-env，实际 %q", v)
	}
}

func TestLoadDotEnvMissingFileIsSilent(t *testing.T) {
	got, err := LoadDotEnv(filepath.Join(t.TempDir(), "not-exists.env"))
	if err != nil {
		t.Fatalf("文件不存在时不应报错: %v", err)
	}
	if got != "" {
		t.Errorf("未加载时期望返回空串，实际 %q", got)
	}
}

// TestEnsureLocalTemplate 验证模板可自动生成，且不会覆盖默认配置。
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

func unsetAll(keys []string) {
	for _, k := range keys {
		_ = os.Unsetenv(k)
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
	if err := cfg.Save(filepath.Join(cfgDir, "local.yaml")); err != nil {
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
