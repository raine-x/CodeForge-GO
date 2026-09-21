// state_test.go 的测试隔离：把用户级运行状态文件重定向到临时目录。
//
// state.yaml 与 data.db 一样是「用户级、跨工作区共享」的，测试若不管它，
// 每个用例都会写真实用户目录下的那份 —— 上一阶段就因为类似共享踩过一次。
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	restore, err := isolateUserLevelFiles()
	if err != nil {
		fmt.Fprintln(os.Stderr, "测试隔离失败: "+err.Error())
		os.Exit(1)
	}
	code := m.Run()
	_ = restore()
	os.Exit(code)
}

// isolateUserLevelFiles 把 state.yaml 指向临时目录，返回恢复函数。
func isolateUserLevelFiles() (func() error, error) {
	dir, err := os.MkdirTemp("", "codeforge-config-test-")
	if err != nil {
		return nil, err
	}
	prev, hadPrev := os.LookupEnv(stateEnvKey)
	if err := os.Setenv(stateEnvKey, filepath.Join(dir, "state.yaml")); err != nil {
		return nil, err
	}
	return func() error {
		if hadPrev {
			_ = os.Setenv(stateEnvKey, prev)
		} else {
			_ = os.Unsetenv(stateEnvKey)
		}
		return os.RemoveAll(dir)
	}, nil
}

// TestSaveStateWritesOnlyOwnedFields 是阶段 2 的核心断言：
// 运行状态落盘时不得带上插件目录与安全规则这些用户/代码维护的字段。
//
// 过去 cfg.Save(local.yaml) 序列化整个 Config，于是 5 条 builtin 插件和 2 条
// 规则被复制进覆盖文件；默认值以后新增条目会被那份旧副本整体替换掉
// （yaml 对切片是替换语义，不是合并）。投影结构里没有这些字段，就不可能复发。
func TestSaveStateWritesOnlyOwnedFields(t *testing.T) {
	cfg := Default()
	cfg.Plugins = []PluginConfig{{Name: "parallel_search", Type: "mcp-http", Enabled: true}}
	cfg.Security.Rules = []SecurityRule{{Tools: []string{"read_file"}, Decision: "allow"}}
	cfg.Security.DenyPatterns = []string{`(?i)rm\s+-rf\s+/`}
	cfg.Agent.WorkDir = `C:/tmp/ws`
	cfg.Agent.MaxSteps = 100
	cfg.Security.PermissionMode = "auto"
	cfg.LLM.Model = "some-model"
	on, off := true, false
	cfg.BuiltinPlugins.SkillCreator = &on
	cfg.BuiltinPlugins.Plan = &off      // 显式关闭要落盘
	cfg.BuiltinPlugins.MultiAgent = nil // 未配置：整键省略，不能写成 null

	if err := cfg.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	raw, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatalf("读取 state.yaml 失败: %v", err)
	}
	got := string(raw)
	for _, want := range []string{"work_dir", "permission_mode", "model", "skill_creator: true", "plan: false"} {
		if !strings.Contains(got, want) {
			t.Errorf("state.yaml 缺少程序自有字段 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "multi_agent") {
		t.Errorf("未配置的开关不该落盘（多 agent 缺省即开启）:\n%s", got)
	}
	// 顶层键按行首匹配，不能用子串：`builtin_plugins:` 里含着 "plugins:"，
	// 子串断言会永远误报（同一个坑在 grep 排查时也踩过一次）。
	hasTopLevelKey := func(key string) bool {
		for _, ln := range strings.Split(got, "\n") {
			if strings.HasPrefix(ln, key) {
				return true
			}
		}
		return false
	}
	for _, banned := range []string{"plugins:", "rules:", "deny_patterns:", "default_decision:"} {
		if hasTopLevelKey(banned) {
			t.Errorf("state.yaml 不应包含归用户/代码的字段 %q:\n%s", banned, got)
		}
	}
	// 「未配置」不能写成 null：local.yaml 里那条 multi_agent: null 就是这么来的，
	// 用户看到它无法判断到底是开、关、还是没设。
	if strings.Contains(got, "null") {
		t.Errorf("state.yaml 不应出现 null 值:\n%s", got)
	}
}

// TestStateOverridesLocal 验证合并次序：default → local → state，
// 界面上最近一次选择要盖过用户手写的静态覆盖。
func TestStateOverridesLocal(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("default.yaml", "agent:\n  work_dir: \"\"\nsecurity:\n  rules:\n    - tools: [\"read_file\"]\n      decision: allow\n")
	write("local.yaml", "agent:\n  work_dir: from-local\nllm:\n  model: from-local\n")
	if err := os.WriteFile(StatePath(), []byte("agent:\n  work_dir: from-state\nllm:\n  model: from-state\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.WorkDir != "from-state" {
		t.Errorf("work_dir 应为 from-state（state 优先级最高），实际 %q", cfg.Agent.WorkDir)
	}
	if cfg.LLM.Model != "from-state" {
		t.Errorf("model 应为 from-state，实际 %q", cfg.LLM.Model)
	}
	// state.yaml 不带 rules → default.yaml 的规则必须原样保留，不被空副本截断。
	if len(cfg.Security.Rules) != 1 {
		t.Errorf("规则数应为 1（来自 default.yaml），实际 %d", len(cfg.Security.Rules))
	}
}

// TestStateSaveLoadRoundTripKeepsDefaults 是那次「一次点击写脏整份配置」的最小复现：
// 保存运行状态后再加载，用户侧默认值不能被状态文件里的副本改掉。
func TestStateSaveLoadRoundTripKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(
		"security:\n  permission_mode: ask\n  rules:\n"+
			"    - tools: [\"read_file\", \"todo_write\"]\n      decision: allow\n"+
			"    - tools: [\"write_file\", \"web_fetch\"]\n      decision: ask\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	beforeRules := len(cfg.Security.Rules)

	// 模拟「设置页点一下权限模式」——新实现只写 state.yaml。
	cfg.Security.PermissionMode = "auto"
	if err := cfg.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "local.yaml")); !os.IsNotExist(err) {
		t.Error("程序不应再创建/改写 local.yaml")
	}

	again, err := Load(dir)
	if err != nil {
		t.Fatalf("re-Load: %v", err)
	}
	if again.Security.PermissionMode != "auto" {
		t.Errorf("权限模式未持久化，实际 %q", again.Security.PermissionMode)
	}
	if len(again.Security.Rules) != beforeRules {
		t.Errorf("规则条数被状态文件改掉了: %d → %d", beforeRules, len(again.Security.Rules))
	}
}
