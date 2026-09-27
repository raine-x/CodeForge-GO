// builtin/goal.go 实现目标模式的工具侧：goal_verify。
//
// 职责边界（刻意很窄）：goal_verify **只判定，不修复**。
// 它跑一轮独立审查者，把结论记进目标账本，然后原样把证据与判定交回主智能体。
// 修复动作由主智能体做 —— 它有完整上下文，且用户能随时插话/打断；
// 若把修复也塞进这一次工具调用，用户就会在一次长达数分钟的自主循环里失去控制。
package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"codeforge/pkg/tools"
)

// GoalVerifierRunner 是审查者调度接口（由 agent.GoalVerifier 实现）。
type GoalVerifierRunner interface {
	Verify(ctx context.Context, goal string, priorFindings string) tools.GoalVerifyOutcome
}

// GoalVerifyTool 是目标验证工具。
//
// 轮数上限可热更新（设置页在插件面板里能改），所以 Description / InputSchema
// 里的数字都从当前值生成 —— 写死「最多 5 次」而实际被调成 2，模型会按 5 次
// 规划然后被拒，白跑一轮。
type GoalVerifyTool struct {
	runner GoalVerifierRunner

	mu        sync.RWMutex
	maxRounds int // 0 表示用缺省值
}

// NewGoalVerifyTool 构造目标验证工具。
func NewGoalVerifyTool(runner GoalVerifierRunner) *GoalVerifyTool {
	return &GoalVerifyTool{runner: runner}
}

// SetMaxRounds 更新自循环轮数上限（设置页热更新）。
func (t *GoalVerifyTool) SetMaxRounds(n int) {
	t.mu.Lock()
	t.maxRounds = n
	t.mu.Unlock()
}

func (t *GoalVerifyTool) limit() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.maxRounds <= 0 {
		return defaultGoalRounds
	}
	if t.maxRounds > maxGoalRoundsCap {
		return maxGoalRoundsCap
	}
	return t.maxRounds
}

// 轮数兜底与硬顶（与 config 侧口径一致；此处独立定义是为了让本包不 import config）。
const (
	defaultGoalRounds = 5
	maxGoalRoundsCap  = 20
)

func (t *GoalVerifyTool) Name() string { return "goal_verify" }

func (t *GoalVerifyTool) Description() string {
	return fmt.Sprintf("让一个独立的验证审查者真的把改动跑起来，判断「目标是否达成」，"+
		"并给出带证据的结论（通过/未通过/无法观察/无可观察面）。"+
		"仅在用户输入了 @goal_mode 之后使用：动手改完就调一次，没通过就按返回的发现清单"+
		"修完再调，最多 %d 轮。"+
		"没有它你就是「自己写完自己说完成了」—— 那不算验证。", t.limit())
}

func (t *GoalVerifyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"goal":{"type":"string","description":"可判定的目标：达到什么程度算达成（要具体到可观察）"},` +
		`"note":{"type":"string","description":"可选：给审查者的补充说明（怎么跑起来、去哪看效果）"}` +
		`},"required":["goal"]}`)
}

// ToolTimeout 申请比默认 120s 更宽的兜底超时。
//
// 一次调用内部要跑完一整轮审查者：多次 LLM 往返，还可能编译、起服务、等它起来。
// 120s 一到，工具连同它发起的嵌套 LLM 请求一起被砍，调用方只看到一句
// 「工具执行超时或被取消」，什么也没发生。
//
// ⚠️ 这是**兜底**，不是业务上限 —— 业务上限是 maxRounds（轮数）与审查者步数预算。
// 用户点停止照样立刻中断（cctx 派生自 ctx）。
func (t *GoalVerifyTool) ToolTimeout(json.RawMessage) time.Duration {
	// 给足余量：审查者单工具 5 分钟、步数上限 30，这里按「跑完所有步也不该被杀」
	// 取一个明显更宽的值。宁可慢，不要在结论出来前被砍。
	return 60 * time.Minute
}

func (t *GoalVerifyTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if t.runner == nil {
		return tools.Err("目标验证未就绪（审查者调度器为空）"), nil
	}
	var in struct {
		Goal string `json:"goal"`
		Note string `json:"note"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return tools.Err("参数解析失败：%v", err), nil
	}
	goal := strings.TrimSpace(in.Goal)
	if goal == "" {
		return tools.Err("必须给出 goal：达到什么程度算达成"), nil
	}

	// 账本必须由服务端装进 ctx（tools.WithGoalLedger），指向**本轮那个会话**。
	// 工具不能自己造 Session —— 记到别处的话，注入的提示段永远为空，
	// 「目标未通过就不许宣布完成」这条约束就静默失效了。
	ledger, ok := tools.GoalLedgerFrom(ctx)
	if !ok {
		return tools.Err("取不到当前会话的目标账本（内部错误，不该由你重试）"), nil
	}

	before := ledger.Goal()
	// 开目标。⚠️ 轮次不在这里重置：改写目标措辞不是换目标（由账本自己判别）。
	ledger.SetGoal(goal, t.limit(), stepOf(ctx))

	// 预算已用尽：不再跑一次审查（那只是白烧一次额度），直接把问题交回用户。
	//
	// 刻意**不**在这里返回错误：那会让模型以为「工具坏了」从而重试或换路走，
	// 而正确反应是「停下来告诉用户需要人工介入」。
	if before.Open() && before.Exhausted() {
		return tools.Ok(map[string]any{
			"verdict":       string(tools.VerdictBlocked),
			"goal":          goal,
			"round":         before.Round,
			"max_rounds":    before.MaxRounds,
			"exhausted":     true,
			"instruction":   budgetOutInstruction(before),
			"last_findings": before.LastFindings,
		}), nil
	}

	prior := ""
	if before.Open() {
		prior = before.LastFindings
	}
	out := t.runner.Verify(ctx, goalWithNote(goal, strings.TrimSpace(in.Note)), prior)

	findings := extractFindings(out.Report)
	after := ledger.RecordVerdict(out.Verdict, findings, stepOf(ctx))

	payload := map[string]any{
		"verdict":    string(out.Verdict),
		"goal":       goal,
		"report":     out.Report,
		"round":      after.Round,
		"max_rounds": after.MaxRounds,
		"remaining":  after.Remaining(),
	}
	if out.Err != nil {
		payload["error"] = out.Err.Error()
	}
	if findings != "" {
		payload["findings"] = findings
	}
	if out.Verdict.Passes() {
		payload["message"] = "目标已验证通过。可以如实告诉用户结果，并附上审查者的证据。"
	} else if after.Exhausted() {
		payload["exhausted"] = true
		payload["instruction"] = budgetOutInstruction(after)
	} else {
		payload["instruction"] = "目标尚未验证通过：**先按「发现」修掉，再重新调用本工具**。" +
			"不要在没有实际改动的情况下重复调用（那只是原地打转、白烧额度）。"
	}
	return tools.Ok(payload), nil
}

// budgetOutInstruction 是预算用尽时给主智能体的明确指令。
//
// 措辞里必须写「不要宣布完成」和「需要人工介入」：预算用尽时最容易发生的
// 错误是模型硬撑 —— 或者更糟，改口说「基本完成了」。
func budgetOutInstruction(g tools.GoalState) string {
	return fmt.Sprintf("自循环预算已用尽（%d/%d 轮）——**不要再调用目标验证，也不要宣布完成**。"+
		"把仍未解决的问题连同审查者的证据一并告诉用户，说明需要人工介入。", g.Round, g.MaxRounds)
}

// goalWithNote 把补充说明并进目标描述。
func goalWithNote(goal, note string) string {
	if note == "" {
		return goal
	}
	return goal + "\n（审查者补充：怎么跑起来、去哪看效果 —— " + note + "）"
}

// extractFindings 从审查者报告里摘出发现清单。
//
// 判定行之后的段落就是发现（ParseVerdict 已把判定行之后的尾巴回传）；
// 若报告很短或没有可用段落，就整段作为发现（总比丢掉强）。
func extractFindings(report string) string {
	r := strings.TrimSpace(report)
	if r == "" {
		return ""
	}
	lines := strings.Split(r, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if _, ok := tools.IsVerdictLine(lines[i]); ok {
			tail := strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			if tail != "" {
				return tail
			}
			break
		}
	}
	return r
}

// stepOf 从 ctx 取出当前会话步号（用于账本记录进展；取不到记 0）。
func stepOf(ctx context.Context) int {
	sc, ok := tools.SessionFrom(ctx)
	if !ok {
		return 0
	}
	return sc.Step
}

// RegisterGoalVerify 注册目标验证工具，并返回实例供设置页热更新轮数上限。
func RegisterGoalVerify(r *tools.Registry, runner GoalVerifierRunner) *GoalVerifyTool {
	tool := NewGoalVerifyTool(runner)
	r.Register(tool)
	return tool
}
