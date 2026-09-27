package config

import "testing"

// 目标模式的轮数上下界。这组测试锁的是「用户能在设置页调，但不能调出失控值」。
func TestGoalModeRounds(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"未配置（0）→ 缺省 5", 0, GoalModeDefaultRounds},
		{"负数 → 缺省 5", -3, GoalModeDefaultRounds},
		{"正常值 3 → 3", 3, 3},
		{"上界 20 → 20", GoalModeMaxRoundsCap, GoalModeMaxRoundsCap},
		{"超上界 9999 → 夹到 20", 9999, GoalModeMaxRoundsCap},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := BuiltinPluginsConfig{GoalModeMaxRounds: c.in}
			if got := cfg.GoalModeRounds(); got != c.want {
				t.Errorf("配置 %d → 期望 %d，实际 %d", c.in, c.want, got)
			}
		})
	}
}

// 0 必须被归一成缺省值而不是当 0 用：0 轮意味着 goal_verify 第一次调用就报
// 「预算耗尽」，目标模式形同虚设，却看不出是配置问题。
func TestGoalModeRoundsNeverZero(t *testing.T) {
	for _, in := range []int{0, -1, -100} {
		if got := (BuiltinPluginsConfig{GoalModeMaxRounds: in}).GoalModeRounds(); got < 1 {
			t.Errorf("配置 %d 得到 %d，必须至少为 1 轮", in, got)
		}
	}
}

// GoalMode 开关缺省开启（与其余内置插件一致）。
func TestGoalModeEnabledDefaultsTrue(t *testing.T) {
	if !(BuiltinPluginsConfig{}).GoalModeEnabled() {
		t.Errorf("未配置时目标模式应默认开启")
	}
	off := false
	if (BuiltinPluginsConfig{GoalMode: &off}).GoalModeEnabled() {
		t.Errorf("显式写 false 时应关闭")
	}
}

// 目标模式的开关与轮数都要能经 state.yaml 往返 —— 设置页改的就是这两个字段。
func TestGoalModeStateRoundTrip(t *testing.T) {
	off := false
	rounds := 9
	cfg := &Config{}
	cfg.BuiltinPlugins.GoalMode = &off
	cfg.BuiltinPlugins.GoalModeMaxRounds = rounds

	s := cfg.stateProjection()
	if s.BuiltinPlugins == nil {
		t.Fatalf("内置插件段不应为 nil")
	}
	if s.BuiltinPlugins.GoalMode == nil || *s.BuiltinPlugins.GoalMode {
		t.Errorf("goal_mode 开关未正确投影")
	}
	if s.BuiltinPlugins.GoalModeMaxRounds != rounds {
		t.Errorf("轮数未正确投影：期望 %d，实际 %d", rounds, s.BuiltinPlugins.GoalModeMaxRounds)
	}
}

// 什么都没配时不应凭空写出 builtin_plugins 段（state.yaml 只留真要说的话）。
func TestGoalModeStateOmittedWhenUnset(t *testing.T) {
	cfg := &Config{}
	if s := cfg.stateProjection(); s.BuiltinPlugins != nil {
		t.Errorf("全未配置时不应写出 builtin_plugins 段，实际 %+v", s.BuiltinPlugins)
	}
}

// 只配轮数（开关留 nil）也要建出这一段 —— 否则设置页改了轮数却存不进去。
func TestGoalModeRoundsAloneCreatesSection(t *testing.T) {
	cfg := &Config{}
	cfg.BuiltinPlugins.GoalModeMaxRounds = 7
	s := cfg.stateProjection()
	if s.BuiltinPlugins == nil {
		t.Fatalf("只配轮数时也应写出 builtin_plugins 段")
	}
	if s.BuiltinPlugins.GoalModeMaxRounds != 7 {
		t.Errorf("轮数未写入：%+v", s.BuiltinPlugins)
	}
}
