package server

import (
	"testing"

	"codeforge/pkg/agent"
)

// 上下文占用应当「随对话实时增长」，而不是等本轮结束才跳一下。
//
// 2026-09-21 反馈：底部进度条只在新建/切换会话与本轮 idle 时刷新，
// 轮次进行中一直不动 —— 用户看不到上下文正在被消耗，也就无从预判
// 什么时候会触发压缩。
func TestPushesContextOnProgressEvents(t *testing.T) {
	should := []string{
		agent.EventUser,       // 用户刚发的话进了历史
		agent.EventStep,       // 步骤边界
		agent.EventToolResult, // 占用增长的主要来源（读文件/跑命令的输出）
		agent.EventCompress,   // 压缩后占用回落，必须立刻反映
	}
	for _, ev := range should {
		if !pushesContextOn(ev) {
			t.Errorf("事件 %q 之后应刷新上下文占用", ev)
		}
	}
}

// delta 类事件每秒几十上百条，每次都重算全历史会白白烧 CPU —— 必须排除。
func TestPushesContextSkipsHighFrequencyEvents(t *testing.T) {
	skip := []string{
		agent.EventText,        // 流式正文
		agent.EventToolPending, // 工具参数流式生成中
		agent.EventToolCall,    // 工具调用刚解析出来（尚未执行，占用未变）
		"reasoning",            // 流式思考（agent 里是字面量，未导出常量）
		agent.EventDone,
		agent.EventError,
		agent.EventRetry,
	}
	for _, ev := range skip {
		if pushesContextOn(ev) {
			t.Errorf("事件 %q 不该触发上下文刷新（高频事件会白烧 CPU）", ev)
		}
	}
}

// 空字符串（未知事件）不该误触发。
func TestPushesContextOnUnknown(t *testing.T) {
	if pushesContextOn("") || pushesContextOn("某个未来才加的事件") {
		t.Error("未知事件不该触发上下文刷新")
	}
}
