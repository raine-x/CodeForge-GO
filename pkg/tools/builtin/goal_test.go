package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/tools"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeLedger 是内存账本（不必开 SQLite 就能测工具行为）。
type fakeLedger struct {
	st       tools.GoalState
	setCalls int
	recCalls int
	lastFind string
	lastVerd tools.GoalVerdict
}

func (f *fakeLedger) SetGoal(goal string, maxRounds, step int) tools.GoalState {
	f.setCalls++
	f.st.Goal = strings.TrimSpace(goal)
	if maxRounds > 0 {
		f.st.MaxRounds = maxRounds
	}
	f.st.LastStep = step
	return f.st
}

func (f *fakeLedger) Goal() tools.GoalState { return f.st }

func (f *fakeLedger) RecordVerdict(v tools.GoalVerdict, findings string, step int) tools.GoalState {
	f.recCalls++
	f.lastVerd = v
	f.lastFind = findings
	f.st.Round++
	f.st.LastVerdict = v
	f.st.LastFindings = findings
	f.st.LastStep = step
	if v.Passes() {
		f.st.Goal = ""
	}
	return f.st
}

// newLedgerFrom 把假账本装进 ctx（与真实现同一个入口：tools.WithGoalLedger）。
func newLedgerFrom(ctx context.Context, f *fakeLedger) context.Context {
	return tools.WithGoalLedger(ctx, &fakeLedgerAdapter{f})
}

// fakeLedgerAdapter 把假账本适配成 tools.GoalLedger。
type fakeLedgerAdapter struct{ f *fakeLedger }

func (a *fakeLedgerAdapter) SetGoal(goal string, maxRounds, step int) tools.GoalState {
	return a.f.SetGoal(goal, maxRounds, step)
}

func (a *fakeLedgerAdapter) Goal() tools.GoalState { return a.f.Goal() }

func (a *fakeLedgerAdapter) RecordVerdict(v tools.GoalVerdict, findings string, step int) tools.GoalState {
	return a.f.RecordVerdict(v, findings, step)
}

// fakeRunner 是审查者替身。
type fakeRunner struct {
	out       tools.GoalVerifyOutcome
	calls     int
	lastGoal  string
	lastPrior string
}

func (f *fakeRunner) Verify(_ context.Context, goal, prior string) tools.GoalVerifyOutcome {
	f.calls++
	f.lastGoal = goal
	f.lastPrior = prior
	return f.out
}

// goalSessionKey 是测试用来注入假账本的私有键。
// 生产路径不走这里（真账本由 main 层通过 SessionLedger 的钩子提供）。
type goalSessionKey struct{}

func ctxWithSession(t *testing.T) context.Context {
	t.Helper()
	return tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1", Step: 4})
}

// resultData 把 Ok(...) 的 map 取出来。
func resultData(t *testing.T, res *tools.ToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatalf("结果为 nil")
	}
	if !res.Success {
		t.Fatalf("期望成功，实际: %+v", res)
	}
	m, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("结果不是 map: %T", res.Data)
	}
	return m
}

// ---------------------------------------------------------------------------
// 基本行为
// ---------------------------------------------------------------------------

func TestGoalVerifyBasics(t *testing.T) {
	t.Run("缺 goal → 失败并说明原因", func(t *testing.T) {
		tool := NewGoalVerifyTool(&fakeRunner{})
		res, _ := tool.Execute(ctxWithSession(t), json.RawMessage(`{}`))
		if res.Success {
			t.Errorf("缺 goal 应失败")
		}
		if !strings.Contains(res.Error, "goal") {
			t.Errorf("错误信息应点明缺 goal: %q", res.Error)
		}
	})

	t.Run("空 goal 字符串（含空白）→ 同样拒绝", func(t *testing.T) {
		tool := NewGoalVerifyTool(&fakeRunner{})
		res, _ := tool.Execute(ctxWithSession(t), json.RawMessage(`{"goal":"   "}`))
		if res.Success {
			t.Errorf("空白 goal 应被拒绝")
		}
	})

	t.Run("参数解析失败 → 返回可读错误而非 panic", func(t *testing.T) {
		tool := NewGoalVerifyTool(&fakeRunner{})
		res, _ := tool.Execute(ctxWithSession(t), json.RawMessage(`{不是json`))
		if res.Success {
			t.Errorf("坏 JSON 应失败")
		}
	})

	t.Run("无审查者 → 失败", func(t *testing.T) {
		tool := NewGoalVerifyTool(nil)
		res, _ := tool.Execute(ctxWithSession(t), json.RawMessage(`{"goal":"x"}`))
		if res.Success {
			t.Errorf("无审查者应失败")
		}
	})

	t.Run("note 并入目标描述", func(t *testing.T) {
		r := &fakeRunner{out: tools.GoalVerifyOutcome{Verdict: tools.VerdictPass, Report: "ok\nVERDICT: PASS"}}
		f := &fakeLedger{}
		tool := NewGoalVerifyTool(r)
		ctx := newLedgerFrom(ctxWithSession(t), f)
		if _, err := tool.Execute(ctx, json.RawMessage(`{"goal":"能登录","note":"npm run dev 后看 8080"}`)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(r.lastGoal, "npm run dev") {
			t.Errorf("note 应并进目标描述，实际 %q", r.lastGoal)
		}
	})
}

// ---------------------------------------------------------------------------
// 判定回传：这是整个工具的核心契约
// ---------------------------------------------------------------------------

func TestGoalVerifyRecordsVerdict(t *testing.T) {
	cases := []struct {
		name    string
		verdict tools.GoalVerdict
		wantMsg string
	}{
		{"PASS → 关闭目标并给出可回传给用户的消息", tools.VerdictPass, "验证通过"},
		{"FAIL → 保持开启并要求先修再验", tools.VerdictFail, "先按"},
		{"BLOCKED → 同样不放过", tools.VerdictBlocked, "先按"},
		{"SKIP → 不放过", tools.VerdictSkip, "先按"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &fakeRunner{out: tools.GoalVerifyOutcome{
				Verdict: c.verdict,
				Report:  "做了这些…\n观察到这个…\nVERDICT: " + string(c.verdict) + "\n- 【登录页】点了登录 → 仍 500 → 说明没修好",
			}}
			f := &fakeLedger{}
			tool := NewGoalVerifyTool(r)
			ctx := newLedgerFrom(ctxWithSession(t), f)
			res, _ := tool.Execute(ctx, json.RawMessage(`{"goal":"能正常登录"}`))
			d := resultData(t, res)

			if f.lastVerd != c.verdict {
				t.Errorf("账本应记 %q，实际 %q", c.verdict, f.lastVerd)
			}
			if f.recCalls != 1 {
				t.Errorf("应记账一次，实际 %d", f.recCalls)
			}
			msg, _ := d["instruction"].(string)
			if msg == "" {
				msg, _ = d["message"].(string)
			}
			if !strings.Contains(msg, c.wantMsg) {
				t.Errorf("回传指令应含 %q，实际 %q", c.wantMsg, msg)
			}
		})
	}
}

// 发现清单必须被单独摘出来 —— 它是主智能体的修复依据。
func TestGoalVerifyExtractsFindings(t *testing.T) {
	r := &fakeRunner{out: tools.GoalVerifyOutcome{
		Verdict: tools.VerdictFail,
		Report:  "我跑了 npm run dev，页面加载正常。\nVERDICT: FAIL\n- 【登录】提交空表单 → 无提示静默失败 → 缺校验",
	}}
	f := &fakeLedger{}
	tool := NewGoalVerifyTool(r)
	ctx := newLedgerFrom(ctxWithSession(t), f)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"goal":"能登录"}`))
	d := resultData(t, res)

	find, _ := d["findings"].(string)
	if !strings.Contains(find, "缺校验") {
		t.Errorf("发现清单应含具体问题，实际 %q", find)
	}
	if strings.Contains(find, "VERDICT") {
		t.Errorf("发现清单里不该残留判定行: %q", find)
	}
}

// 轮次要推进，且 remaining 随之下减。
func TestGoalVerifyRoundAccounting(t *testing.T) {
	f := &fakeLedger{st: tools.GoalState{Goal: "老目标", Round: 1, MaxRounds: 5,
		LastFindings: "上一轮：还是报错"}}
	r := &fakeRunner{out: tools.GoalVerifyOutcome{Verdict: tools.VerdictFail, Report: "x\nVERDICT: FAIL\n- 还是不行"}}
	tool := NewGoalVerifyTool(r)
	ctx := newLedgerFrom(ctxWithSession(t), f)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"goal":"新措辞的目标"}`))
	d := resultData(t, res)

	if got := d["round"]; got != 2 {
		t.Errorf("轮次应递增到 2，实际 %v", got)
	}
	if got := d["remaining"]; got != 3 {
		t.Errorf("剩余应为 3，实际 %v", got)
	}
	// 上一轮发现要传给审查者，让它重点确认是否已修
	if !strings.Contains(r.lastPrior, "还是报错") {
		t.Errorf("应把上一轮发现传给审查者，实际 %q", r.lastPrior)
	}
}

// 预算用尽：不再跑审查，直接交回用户。
func TestGoalVerifyStopsWhenExhausted(t *testing.T) {
	f := &fakeLedger{st: tools.GoalState{Goal: "老目标", Round: 5, MaxRounds: 5,
		LastFindings: "五轮都没修好"}}
	r := &fakeRunner{out: tools.GoalVerifyOutcome{Verdict: tools.VerdictPass, Report: "VERDICT: PASS"}}
	tool := NewGoalVerifyTool(r)
	ctx := newLedgerFrom(ctxWithSession(t), f)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"goal":"再试一次"}`))
	d := resultData(t, res)

	if r.calls != 0 {
		t.Errorf("预算用尽时不该再跑审查（那只是白烧一次额度），实际跑了 %d 次", r.calls)
	}
	if d["exhausted"] != true {
		t.Errorf("应标记预算已用尽")
	}
	inst, _ := d["instruction"].(string)
	for _, want := range []string{"不要", "宣布完成", "人工介入"} {
		if !strings.Contains(inst, want) {
			t.Errorf("用尽指令应含 %q，实际 %q", want, inst)
		}
	}
	if find, _ := d["last_findings"].(string); !strings.Contains(find, "五轮都没修好") {
		t.Errorf("应带上遗留问题让用户看到，实际 %q", find)
	}
}

// 预算用尽时**不能**判 PASS —— 否则模型会拿一个「跳过审查」的假通过去宣布完成。
func TestGoalVerifyNeverPassesWhenExhausted(t *testing.T) {
	f := &fakeLedger{st: tools.GoalState{Goal: "x", Round: 9, MaxRounds: 5}}
	tool := NewGoalVerifyTool(&fakeRunner{})
	ctx := newLedgerFrom(ctxWithSession(t), f)
	res, _ := tool.Execute(ctx, json.RawMessage(`{"goal":"x"}`))
	d := resultData(t, res)
	if v, _ := d["verdict"].(string); v == string(tools.VerdictPass) {
		t.Errorf("跳过审查不得判 PASS")
	}
}

// ---------------------------------------------------------------------------
// 超时与描述的动态性
// ---------------------------------------------------------------------------

// 必须比默认 120s 宽得多：一次调用内部要跑完一整轮审查者。
func TestGoalVerifyAsksForWiderTimeout(t *testing.T) {
	tool := NewGoalVerifyTool(&fakeRunner{})
	var tp tools.TimeoutPolicy = tool
	d := tp.ToolTimeout(json.RawMessage(`{}`))
	if d <= 120*time.Second {
		t.Errorf("申请的超时（%v）应远宽于默认 120s", d)
	}
	// 也要确认它真的实现了接口（执行器靠类型断言识别）
	if _, ok := tools.Tool(tool).(tools.TimeoutPolicy); !ok {
		t.Errorf("goal_verify 必须实现 tools.TimeoutPolicy，否则执行器不会放宽")
	}
}

// 轮数上限热更新后，描述与 schema 里的数字要跟着变。
// 写死「最多 5 次」而实际是 2，模型会按 5 次规划然后被拒，白跑一轮。
func TestGoalVerifyDescriptionTracksRounds(t *testing.T) {
	tool := NewGoalVerifyTool(&fakeRunner{})
	if !strings.Contains(tool.Description(), "最多 5 轮") {
		t.Errorf("默认应显示 5 轮，实际: %s", tool.Description())
	}
	tool.SetMaxRounds(2)
	if !strings.Contains(tool.Description(), "最多 2 轮") {
		t.Errorf("热更新后描述应显示 2 轮，实际: %s", tool.Description())
	}
	// 越界要夹住，不能让设置页填出「最多 99999 轮」
	tool.SetMaxRounds(99999)
	if !strings.Contains(tool.Description(), "最多 20 轮") {
		t.Errorf("越界应夹到 20 轮，实际: %s", tool.Description())
	}
}

func TestGoalVerifyDescriptionMentionsTrigger(t *testing.T) {
	tool := NewGoalVerifyTool(&fakeRunner{})
	d := tool.Description()
	if !strings.Contains(d, "@goal_mode") {
		t.Errorf("描述应说明触发条件（否则模型会在没触发时乱用）")
	}
}

// 名称必须是纯 ASCII：注册表会把非 ASCII 名字改写成 wire 名再映射回来，
// 多一层映射就多一处可能出错的地方。
func TestGoalVerifyNameIsASCII(t *testing.T) {
	tool := NewGoalVerifyTool(&fakeRunner{})
	if tool.Name() != "goal_verify" {
		t.Errorf("工具名应为 goal_verify，实际 %q", tool.Name())
	}
	for _, r := range tool.Name() {
		if r > 127 {
			t.Errorf("工具名含非 ASCII 字符 %q，会被 wire 名映射改写", r)
		}
	}
}

func TestGoalVerifySchemaRequiresGoal(t *testing.T) {
	tool := NewGoalVerifyTool(&fakeRunner{})
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON: %v", err)
	}
	if len(schema.Required) == 0 || schema.Required[0] != "goal" {
		t.Errorf("goal 应是必填，实际 required=%v", schema.Required)
	}
}

func TestRegisterGoalVerify(t *testing.T) {
	reg := tools.NewRegistry()
	tool := RegisterGoalVerify(reg, &fakeRunner{})
	if _, ok := reg.Get("goal_verify"); !ok {
		t.Fatalf("注册后应能取到")
	}
	if tool == nil {
		t.Errorf("应返回工具实例供热更新")
	}
}
