package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 硬上限必须与 pkg/agent.MaxSubagents 同源：这里被改小/改大，调度器校验会跟着变。
func TestSubagentConcurrencyCapIsStable(t *testing.T) {
	if SubagentConcurrencyCap != 5 {
		t.Fatalf("并发硬上限期望 5（前端滑条 max 与文档都写死 5），实际 %d", SubagentConcurrencyCap)
	}
}

// 并发上限：缺省 / 0 / 负数 / 超过硬上限 一律回落硬上限，只有 1..cap 之间的值被采纳。
func TestSubagentMaxConcurrentClamps(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{"缺省（未配置）回落硬上限", Config{}, SubagentConcurrencyCap},
		{"0 回落硬上限", Config{Subagents: SubagentConfig{MaxConcurrent: 0}}, SubagentConcurrencyCap},
		{"负数回落硬上限", Config{Subagents: SubagentConfig{MaxConcurrent: -3}}, SubagentConcurrencyCap},
		{"超过硬上限回落硬上限", Config{Subagents: SubagentConfig{MaxConcurrent: SubagentConcurrencyCap + 1}}, SubagentConcurrencyCap},
		{"极大值回落硬上限", Config{Subagents: SubagentConfig{MaxConcurrent: 999}}, SubagentConcurrencyCap},
		{"等于硬上限被采纳", Config{Subagents: SubagentConfig{MaxConcurrent: SubagentConcurrencyCap}}, SubagentConcurrencyCap},
		{"区间内被采纳（1）", Config{Subagents: SubagentConfig{MaxConcurrent: 1}}, 1},
		{"区间内被采纳（2）", Config{Subagents: SubagentConfig{MaxConcurrent: 2}}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.SubagentMaxConcurrent(); got != c.want {
				t.Errorf("SubagentMaxConcurrent() 期望 %d，实际 %d", c.want, got)
			}
		})
	}
}

// 步数上限：未配置（<=0）继承主 loop；显式配置夹在 1..硬上限，越界按硬上限收敛。
//
// 「继承」是有意设计：早先子智能体被写死 8 步，与主 loop 毫无关系，探索类任务
// 经常在半途被硬停，模型被迫交一份「还没看完」的结论。
func TestSubagentMaxStepsInheritsWhenUnset(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		parent int
		want   int
	}{
		{"缺省继承主循环", Config{}, 25, 25},
		{"0 继承主循环", Config{Subagents: SubagentConfig{MaxSteps: 0}}, 12, 12},
		{"负数继承主循环", Config{Subagents: SubagentConfig{MaxSteps: -5}}, 30, 30},
		{"主循环未配置时给可运行兜底", Config{}, 0, 8},
		{"显式调小被采纳", Config{Subagents: SubagentConfig{MaxSteps: 4}}, 25, 4},
		{"显式等于硬上限被采纳", Config{Subagents: SubagentConfig{MaxSteps: SubagentStepCap}}, 25, SubagentStepCap},
		{"超过硬上限收敛（不拒绝保存）", Config{Subagents: SubagentConfig{MaxSteps: 9999}}, 25, SubagentStepCap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.cfg.SubagentMaxSteps(c.parent); got != c.want {
				t.Errorf("SubagentMaxSteps(parent=%d) 期望 %d，实际 %d", c.parent, c.want, got)
			}
		})
	}
}

// 未配置 max_steps 时 state.yaml 整段不出现该键（省略键 = 不覆盖 default.yaml）。
func TestStateOmitsUnsetSubagentMaxSteps(t *testing.T) {
	t.Setenv(stateEnvKey, filepath.Join(t.TempDir(), "state.yaml"))
	c := Config{Subagents: SubagentConfig{MaxConcurrent: 3}}
	if err := c.SaveState(); err != nil {
		t.Fatalf("SaveState 失败: %v", err)
	}
	data, err := os.ReadFile(StatePath())
	if err != nil {
		t.Fatalf("读取状态文件失败: %v", err)
	}
	if strings.Contains(string(data), "max_steps") {
		t.Fatalf("未配置时不该写出 max_steps 键，实际内容：\n%s", data)
	}
	// 配了才写，且写进去的值能被读回来。
	c.Subagents.MaxSteps = 7
	if err := c.SaveState(); err != nil {
		t.Fatalf("SaveState 失败: %v", err)
	}
	data, _ = os.ReadFile(StatePath())
	if !strings.Contains(string(data), "max_steps: 7") {
		t.Fatalf("配置后应写出 max_steps: 7，实际内容：\n%s", data)
	}
}

// 能力开关三态：nil = 缺省开启；显式 false 才关闭；显式 true 保持开启。
func TestSubagentAllowFlagsTriState(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	type getter struct {
		name string
		fn   func(Config) bool
	}
	getters := []getter{
		{"AllowWrite", Config.SubagentAllowWrite},
		{"AllowDelete", Config.SubagentAllowDelete},
		{"AllowMemory", Config.SubagentAllowMemory},
	}
	set := map[string]func(*SubagentConfig, *bool){
		"AllowWrite":  func(s *SubagentConfig, p *bool) { s.AllowWrite = p },
		"AllowDelete": func(s *SubagentConfig, p *bool) { s.AllowDelete = p },
		"AllowMemory": func(s *SubagentConfig, p *bool) { s.AllowMemory = p },
	}

	for _, g := range getters {
		t.Run(g.name, func(t *testing.T) {
			// 1) 未配置 → 默认开启
			if got := g.fn(Config{}); !got {
				t.Error("未配置时期望默认开启（true）")
			}
			// 2) 显式 false → 关闭
			var sc SubagentConfig
			set[g.name](&sc, boolPtr(false))
			if got := g.fn(Config{Subagents: sc}); got {
				t.Error("显式 false 时期望关闭")
			}
			// 3) 显式 true → 开启
			sc = SubagentConfig{}
			set[g.name](&sc, boolPtr(true))
			if got := g.fn(Config{Subagents: sc}); !got {
				t.Error("显式 true 时期望开启")
			}
		})
	}
}

// 默认配置下（无 local.yaml 覆盖）子智能体应当是可写、可删、可写记忆，并发为硬上限。
func TestDefaultConfigSubagentDefaults(t *testing.T) {
	cfg := Default()
	if got := cfg.SubagentMaxConcurrent(); got != SubagentConcurrencyCap {
		t.Errorf("默认配置并发期望 %d，实际 %d", SubagentConcurrencyCap, got)
	}
	if !cfg.SubagentAllowWrite() {
		t.Error("默认配置应允许子智能体写入文件")
	}
	if !cfg.SubagentAllowDelete() {
		t.Error("默认配置应允许子智能体删除文件")
	}
	if !cfg.SubagentAllowMemory() {
		t.Error("默认配置应允许子智能体写入记忆")
	}
}

// boolDefault 是「缺省为真」语义的唯一实现，单独钉住边界。
func TestBoolDefault(t *testing.T) {
	tr, fa := true, false
	if !boolDefault(nil, true) {
		t.Error("nil + def=true 期望 true")
	}
	if boolDefault(nil, false) {
		t.Error("nil + def=false 期望 false")
	}
	if !boolDefault(&tr, false) {
		t.Error("显式 true 应覆盖 def=false")
	}
	if boolDefault(&fa, true) {
		t.Error("显式 false 应覆盖 def=true")
	}
}
