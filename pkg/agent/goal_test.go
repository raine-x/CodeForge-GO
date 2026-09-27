package agent

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 账本状态机
// ---------------------------------------------------------------------------

func TestGoalStateOpenAndExhausted(t *testing.T) {
	t.Run("零值未开启", func(t *testing.T) {
		var g GoalState
		if g.Open() {
			t.Errorf("零值不该算开启")
		}
		if got := g.PromptSection(); got != "" {
			t.Errorf("未开启时不该注入提示，实际 %q", got)
		}
	})

	t.Run("开启后未到上限", func(t *testing.T) {
		g := GoalState{Goal: "能正常登录", Round: 2, MaxRounds: 5}
		if !g.Open() {
			t.Errorf("应算开启")
		}
		if g.Exhausted() {
			t.Errorf("2/5 不该算用尽")
		}
		if g.Remaining() != 3 {
			t.Errorf("剩余应为 3，实际 %d", g.Remaining())
		}
	})

	t.Run("恰好到上限", func(t *testing.T) {
		g := GoalState{Goal: "x", Round: 5, MaxRounds: 5}
		if !g.Exhausted() {
			t.Errorf("5/5 应算用尽")
		}
		if g.Remaining() != 0 {
			t.Errorf("剩余应为 0，实际 %d", g.Remaining())
		}
	})

	t.Run("上限被调小到已用轮数之下 → 仍算用尽", func(t *testing.T) {
		// 用 >= 而不是 ==：运行中被把 max_rounds 从 5 改成 2、而已经验了 3 轮时，
		// 若按 == 判断会认为「还剩 1 轮」，于是放行一次本该被拦住的验证。
		g := GoalState{Goal: "x", Round: 3, MaxRounds: 2}
		if !g.Exhausted() {
			t.Errorf("上限调小后应判定为用尽")
		}
	})
}

func TestSessionGoalLedger(t *testing.T) {
	s := &Session{ID: "s1"}

	t.Run("首次开启", func(t *testing.T) {
		g := s.SetGoal("登录后不再报错", 5, 7)
		if !g.Open() || g.Goal != "登录后不再报错" || g.Round != 0 {
			t.Fatalf("首次开启异常: %+v", g)
		}
		if g.LastStep != 7 {
			t.Errorf("应记录步骤 7，实际 %d", g.LastStep)
		}
	})

	t.Run("改写目标文本不重置轮次", func(t *testing.T) {
		// 这是防「靠反复改写措辞绕过预算」的关键：换措辞不是换目标。
		s.RecordVerdict(VerdictFail, "点了登录还是报错", 8)
		g := s.SetGoal("登录后不再报错（补充：也要能退出）", 5, 9)
		if g.Round != 1 {
			t.Errorf("改写目标后轮次应保持 1，实际 %d", g.Round)
		}
	})

	t.Run("记录 FAIL 保持开启并带出发现", func(t *testing.T) {
		g := s.RecordVerdict(VerdictFail, "  点了登录还是报错\n  控制台 500", 10)
		if !g.Open() {
			t.Errorf("FAIL 后目标应保持开启")
		}
		if g.LastVerdict != VerdictFail {
			t.Errorf("应记住 FAIL，实际 %q", g.LastVerdict)
		}
		if !strings.Contains(g.LastFindings, "控制台 500") {
			t.Errorf("发现清单未保留: %q", g.LastFindings)
		}
		if g.VerifiedAt.IsZero() {
			t.Errorf("应记录验证时刻")
		}
		if g.Round != 2 {
			t.Errorf("轮次应递增到 2，实际 %d", g.Round)
		}
	})

	t.Run("BLOCKED 同样保持开启", func(t *testing.T) {
		g := s.RecordVerdict(VerdictBlocked, "起不来服务，缺依赖", 11)
		if !g.Open() {
			t.Errorf("BLOCKED 不能当作通过，目标应保持开启")
		}
	})

	t.Run("PASS 才关账本", func(t *testing.T) {
		g := s.RecordVerdict(VerdictPass, "", 12)
		if g.Open() {
			t.Errorf("PASS 后目标应关闭")
		}
		if g.Round != 4 {
			t.Errorf("PASS 那次同样花掉一轮，轮次应为 4，实际 %d", g.Round)
		}
		if s.Goal().Open() {
			t.Errorf("账本应已关闭")
		}
	})

	t.Run("重新开启从 0 开始吗（预算是否跨目标重置）", func(t *testing.T) {
		// 关账本后再开一个新目标：轮次应当归零，否则第二个目标一开始就超预算。
		// 这是与「改写措辞不重置」不同的场景 —— 那是同一个目标，这是新目标。
		fresh := &Session{ID: "s2"}
		fresh.SetGoal("目标 A", 5, 1)
		fresh.RecordVerdict(VerdictPass, "", 2)
		g := fresh.SetGoal("目标 B", 5, 3)
		if g.Round != 0 {
			t.Errorf("新目标轮次应归零，实际 %d", g.Round)
		}
		if !g.Open() {
			t.Errorf("新目标应处于开启状态")
		}
	})

	t.Run("ClearGoal 保留轮次", func(t *testing.T) {
		fresh := &Session{ID: "s3"}
		fresh.SetGoal("放弃的目标", 5, 1)
		fresh.RecordVerdict(VerdictFail, "有问题", 2)
		fresh.ClearGoal()
		g := fresh.Goal()
		if g.Open() {
			t.Errorf("ClearGoal 后应关闭")
		}
		if g.LastFindings != "" {
			t.Errorf("ClearGoal 应清掉发现")
		}
	})
}

// 预算用尽时提示必须明确说「停下来问人」，否则模型会继续硬撑。
func TestGoalPromptSectionTellsWhenExhausted(t *testing.T) {
	s := &Session{ID: "s1"}
	s.SetGoal("能正常登录", 2, 1)
	s.RecordVerdict(VerdictFail, "还是报错", 2)
	g := s.RecordVerdict(VerdictFail, "还是报错", 3)
	if !g.Exhausted() {
		t.Fatalf("2/2 应已用尽: %+v", g)
	}
	sec := g.PromptSection()
	for _, want := range []string{"能正常登录", "预算已用尽", "人工介入", "不要"} {
		if !strings.Contains(sec, want) {
			t.Errorf("提示应含 %q，实际:\n%s", want, sec)
		}
	}
}

func TestGoalPromptSectionIndentFindings(t *testing.T) {
	g := GoalState{
		Goal: "目标", MaxRounds: 5, Round: 1,
		LastVerdict:  VerdictFail,
		LastFindings: "第一行\n第二行",
	}
	sec := g.PromptSection()
	if !strings.Contains(sec, "  第一行") || !strings.Contains(sec, "  第二行") {
		t.Errorf("多行发现应逐行缩进（否则第二行会被当成正文），实际:\n%s", sec)
	}
}

func TestGoalCloneIsIndependent(t *testing.T) {
	orig := GoalState{Goal: "x", Round: 1, VerifiedAt: time.Now()}
	cp := orig.Clone()
	cp.Goal = "y"
	cp.Round = 99
	if orig.Goal != "x" || orig.Round != 1 {
		t.Errorf("Clone 必须是独立副本，原值被改了: %+v", orig)
	}
}

// ---------------------------------------------------------------------------
// 触发词
// ---------------------------------------------------------------------------

func TestGoalTriggered(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"请修好登录页 @goal_mode", true},
		{"@goal_mode 修好登录页", true},
		{"请修好登录页 @目标模式", true},
		{"@目标模式", true},
		{"@GOAL_MODE", true},
		{"请修好登录页", false},
		{"目标模式是什么", false},    // 没有 @ 前缀不算触发
		{"看看这个 @goal", false}, // @goal 不是 @goal_mode
		{"email: a@b.com", false},
	}
	for _, c := range cases {
		if got := GoalTriggered(c.in); got != c.want {
			t.Errorf("%q → 期望 %v，实际 %v", c.in, c.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// VERDICT 解析
// ---------------------------------------------------------------------------

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want GoalVerdict
	}{
		{"标准末行", "…观察完毕\nVERDICT: PASS", VerdictPass},
		{"末行加粗", "…\n**VERDICT: FAIL**", VerdictFail},
		{"中文措辞", "证据如下\n判定：BLOCKED", VerdictBlocked},
		{"代码块内", "```\nVERDICT: SKIP\n```", VerdictSkip},
		{"行尾带解释", "VERDICT: FAIL —— 因为登录仍 500", VerdictFail},
		{"空行结尾", "VERDICT: PASS\n\n", VerdictPass},
		{"PASSED 不误判成 PASS", "VERDICT: PASSED", VerdictBlocked},
		{"读不出结论 → BLOCKED", "我觉得应该没问题。", VerdictBlocked},
		{"空输出 → BLOCKED", "", VerdictBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := ParseVerdict(c.in)
			if got != c.want {
				t.Errorf("期望 %q，实际 %q", c.want, got)
			}
		})
	}
}

// 「读不出结论就 BLOCKED」是本功能的核心安全性质：判定读不出来绝不能当通过。
func TestParseVerdictNeverSilentlyPasses(t *testing.T) {
	tricky := []string{
		"我完成了任务。",
		"VERDICT: MAYBE",
		"结论是差不多了",
		"status: pass", // 小写、且不是 VERDICT 行 —— 但 token 仍能认出 PASS
	}
	for _, in := range tricky {
		got, _ := ParseVerdict(in)
		if got == VerdictPass && in != "status: pass" {
			t.Errorf("%q 不该被判为通过，实际 %q", in, got)
		}
	}
}

// 取的是**最后一个**判定 token：审查者可能先说「现在看起来 PASS」再改口。
func TestParseVerdictTakesLastVerdict(t *testing.T) {
	in := "初步看像 PASS，但复测发现仍报错。\nVERDICT: FAIL"
	got, _ := ParseVerdict(in)
	if got != VerdictFail {
		t.Errorf("应取末次判定 FAIL，实际 %q", got)
	}
}
