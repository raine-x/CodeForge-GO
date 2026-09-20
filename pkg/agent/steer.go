// steer.go 实现「中途转向」（steering）：任务跑在半空中，用户追加一条新指令，
// 既不丢已经产生的工具结果，也不必等这一轮跑完 —— 指令在下一个步骤边界并入对话。
//
// 与「打断」（cancel）的分工：打断是让循环在边界处直接收摊；
// 转向是让循环在边界处换方向继续跑。两者都不掐断正在飞行中的那次请求，
// 因为掐断只会留下半截回复，而步骤边界本来就有完整上下文可用。
package agent

// maxSteerPending 是单个会话最多暂存的转向指令条数。
// 上限存在的理由不是省内存，而是「攒太多就没法跑了」：十几条指令一次性注入，
// 模型面对的不是一个转向而是一叠互相冲突的需求，不如让用户等一等。
const maxSteerPending = 8

// SteerResult 是 Steer 的三态结论：调用方（WS 层）据此决定回什么。
// 「没人接收」与「排不下」不能混成一件事 —— 前者该另起一轮，
// 后者只能告诉用户等一下；静默丢弃或顺手另起一轮都会打断正在跑的任务。
type SteerResult int

const (
	// SteerQueued 已入队，运行中的循环会在下一个步骤边界并入。
	SteerQueued SteerResult = iota
	// SteerIdle 该会话当前没有运行中的循环。
	SteerIdle
	// SteerFull 运行中但排队已满。
	SteerFull
)

// IsRunning 报告该会话此刻是否有循环在跑。
// 主动压缩等「从界面外部动会话历史」的动作必须先问一句：
// 与运行中的循环同时改写 Messages / 压缩游标，送模视图会出现半套状态。
func (a *Agent) IsRunning(sessionID string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.running[sessionID]
}

// steerQueue 是一个会话的待注入指令队列。
// 字段由 Agent.runMu 保护，本身不再加锁。
type steerQueue struct {
	pending []string
}

// Steer 把一条新指令排进该会话的转向队列。
func (a *Agent) Steer(sessionID, text string) SteerResult {
	if sessionID == "" || text == "" {
		return SteerIdle
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if !a.running[sessionID] {
		return SteerIdle
	}
	q := a.steers[sessionID]
	if q == nil {
		q = &steerQueue{}
		a.steers[sessionID] = q
	}
	if len(q.pending) >= maxSteerPending {
		return SteerFull
	}
	q.pending = append(q.pending, text)
	return SteerQueued
}

// beginRun 登记一次循环的运行域（同一会话同时只有一轮在跑，
// WS 层已在 run() 里先 stop 过旧任务）。
//
// 开跑前清掉残留队列：上一轮若在「取走指令」与「退出登记」之间被取消，
// 队列里可能还压着几条指令，留着会让这一轮凭空多出几条没人提交过的用户消息。
func (a *Agent) beginRun(sessionID string) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running == nil {
		a.running = map[string]bool{}
	}
	if a.steers == nil {
		a.steers = map[string]*steerQueue{}
	}
	a.running[sessionID] = true
	delete(a.steers, sessionID)
}

// endRun 注销运行域并丢弃未消费的指令。
func (a *Agent) endRun(sessionID string) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	delete(a.running, sessionID)
	delete(a.steers, sessionID)
}

// drainSteer 在步骤边界取走全部待注入指令（无则返回 nil）。
func (a *Agent) drainSteer(sessionID string) []string {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	q := a.steers[sessionID]
	if q == nil || len(q.pending) == 0 {
		return nil
	}
	out := q.pending
	q.pending = nil
	return out
}
