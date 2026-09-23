// steer.go 实现「中途转向」（steering）：任务跑在半空中，用户追加一条新指令，
// 既不丢已经产生的工具结果，也不必等这一轮跑完 —— 指令在下一个步骤边界并入对话。
//
// 与「打断」（cancel）的分工：打断是让循环在边界处直接收摊；
// 转向是让循环在边界处换方向继续跑。两者都不掐断正在飞行中的那次请求，
// 因为掐断只会留下半截回复，而步骤边界本来就有完整上下文可用。
package agent

import (
	"context"
	"fmt"
	"time"
)

// maxSteerPending 是单个会话最多暂存的转向指令条数。
// 上限存在的理由不是省内存，而是「攒太多就没法跑了」：十几条指令一次性注入，
// 模型面对的不是一个转向而是一叠互相冲突的需求，不如让用户等一等。
const maxSteerPending = 8

// OriginSteer 是「运行中转向」注入消息的来源标记（llm.Message.Origin）。
//
// 它要落盘并下发给前端，因此是稳定字符串，不能随手改。
// 用途有两个：后端把插话与「真正的提问」区分开（见 isUserQuestion），
// 前端在历史回放时仍能把它渲染成转向样式而不是普通用户消息。
const OriginSteer = "steer"

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

// runHandoffTimeout 是 beginRunWait 等上一轮让位的上限。
// 上一轮被取消后通常在下一个步骤边界就退出（流事件/工具执行都查 ctx），
// 15 秒足够覆盖最慢的一次在飞请求；超时仍不让位说明有工具卡死，如实报错。
const runHandoffTimeout = 15 * time.Second

// tryBeginRun 尝试登记运行域：已在跑则返回 false（不等待、不清队列）。
func (a *Agent) tryBeginRun(sessionID string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running == nil {
		a.running = map[string]bool{}
	}
	if a.steers == nil {
		a.steers = map[string]*steerQueue{}
	}
	if a.running[sessionID] {
		return false
	}
	a.running[sessionID] = true
	delete(a.steers, sessionID)
	return true
}

// beginRunWait 获取会话的运行权：同一会话同一时刻只允许一个循环。
//
// 这是跨连接互斥的关键：WS 的 c.stop() 只能停**本连接**的旧任务，两个浏览器
// 标签页可以同时驱动同一会话 —— 没有这道闸门，两个 ReAct 循环会并发 append
// 同一个 Session.Messages 并各自全量覆盖写库（Save = DELETE + 全量重插），
// 历史直接错乱（docs/修改.md 已记录该隐患）。
//
// 正常路径是「打断后立刻重发」：WS 层先 cancel 旧轮，这里等它在步骤边界退出
// （50ms 轮询），让位即接续；等不到（工具卡死）或调用方 ctx 结束则报错。
func (a *Agent) beginRunWait(ctx context.Context, sessionID string) error {
	deadline := time.Now().Add(runHandoffTimeout)
	for {
		if a.tryBeginRun(sessionID) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("该会话有任务正在运行且迟迟未退出，请先打断或稍后再试")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
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
