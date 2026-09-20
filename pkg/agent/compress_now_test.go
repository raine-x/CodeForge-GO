package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codeforge/pkg/llm"
)

// 主动压缩（界面上的「立即压缩上下文」）：与自动压缩同一套机制，只是不等撞线。

func compressSession(t *testing.T, a *Agent, turns, runes int) *Session {
	t.Helper()
	sess, err := a.History().Create("", "主动压缩")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = buildTurns(turns, runes)
	if err := a.History().Save(sess.ID); err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestCompressNowSummarizesOlderHistory(t *testing.T) {
	p := &ctxStubProvider{reply: "## 已完成的改动\n- 读过 a.go"}
	a := newEmitTestAgent(t, p)
	a.cfg.ContextTokenBudget = 12000
	sess := compressSession(t, a, 12, 400)

	before := EstimateTokens(a.requestView(sess))
	info, err := a.CompressNow(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("主动压缩失败: %v", err)
	}
	if info.Added <= 0 || info.Summarized <= 0 {
		t.Fatalf("应并入若干条历史，实际 %+v", info)
	}
	if info.After >= info.Before {
		t.Errorf("压缩后不该变大：%+v", info)
	}

	// 落库后的真实状态才是准：游标前进、摘要可见。
	loaded, ok := a.History().Get(sess.ID)
	if !ok {
		t.Fatal("会话读不回来")
	}
	if loaded.compressedUpTo < info.Summarized {
		t.Errorf("压缩游标未推进: %d < %d", loaded.compressedUpTo, info.Summarized)
	}
	if !strings.Contains(loaded.summaryText, "读过 a.go") {
		t.Errorf("摘要未写入会话: %q", loaded.summaryText)
	}
	// 完整历史一条不少：主动压缩只改「送模视图」，用户回看与回滚都照旧。
	if got := len(loaded.Messages); got != len(sess.Messages) {
		t.Errorf("原始历史被改动了: %d → %d 条", len(sess.Messages), got)
	}
	if after := EstimateTokens(a.requestView(loaded)); after >= before {
		t.Errorf("送模视图应变小: %d → %d", before, after)
	}
}

// 只有当前一轮可谈时不该压：把刚问的问题压成摘要，模型就不知道要回答什么了。
func TestCompressNowRefusesWhenNothingToCompress(t *testing.T) {
	a := newEmitTestAgent(t, &ctxStubProvider{reply: "摘要"})
	sess, err := a.History().Create("", "只有一句")
	if err != nil {
		t.Fatal(err)
	}
	sess.Messages = []llm.Message{llm.TextMessage(llm.RoleUser, "就这一句")}
	if err := a.History().Save(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompressNow(context.Background(), sess.ID); !errors.Is(err, ErrCompressNothing) {
		t.Errorf("应返回 ErrCompressNothing，实际: %v", err)
	}
}

// 任务在跑时不允许从外部改压缩游标（会与循环内的压缩互相踩）。
func TestCompressNowRefusesWhileRunning(t *testing.T) {
	a := newEmitTestAgent(t, &ctxStubProvider{reply: "摘要"})
	sess := compressSession(t, a, 12, 400)
	a.beginRun(sess.ID)
	t.Cleanup(func() { a.endRun(sess.ID) })

	if _, err := a.CompressNow(context.Background(), sess.ID); !errors.Is(err, ErrCompressBusy) {
		t.Errorf("应返回 ErrCompressBusy，实际: %v", err)
	}
}

// 摘要失败必须一点不碰会话：半途改游标会让前半段历史既不原文可见、也没进摘要。
func TestCompressNowKeepsSessionOnFailure(t *testing.T) {
	a := newEmitTestAgent(t, &ctxStubProvider{err: errors.New("上游 503")})
	a.cfg.ContextTokenBudget = 12000
	sess := compressSession(t, a, 12, 400)

	if _, err := a.CompressNow(context.Background(), sess.ID); err == nil {
		t.Fatal("摘要失败时应返回错误")
	}
	loaded, ok := a.History().Get(sess.ID)
	if !ok {
		t.Fatal("会话读不回来")
	}
	if loaded.compressedUpTo != 0 || strings.TrimSpace(loaded.summaryText) != "" {
		t.Errorf("失败后压缩状态被改动: upTo=%d summary=%q", loaded.compressedUpTo, loaded.summaryText)
	}
	if len(loaded.Messages) != len(sess.Messages) {
		t.Errorf("原始历史条数变了: %d → %d", len(sess.Messages), len(loaded.Messages))
	}
}

// 会话 ID 为空 / 不存在：直接报错，不去猜「当前会话」。
func TestCompressNowUnknownSession(t *testing.T) {
	a := newEmitTestAgent(t, &ctxStubProvider{reply: "摘要"})
	if _, err := a.CompressNow(context.Background(), "nope"); err == nil ||
		!strings.Contains(err.Error(), "会话不存在") {
		t.Errorf("应报会话不存在，实际: %v", err)
	}
}

// 提示词/工具定义吃满预算时不该假装压缩成功（与主循环同一道上限保护）。
func TestCompressNowBudgetTooSmall(t *testing.T) {
	a := newEmitTestAgent(t, &ctxStubProvider{reply: "摘要"})
	a.cfg.ContextTokenBudget = 2000 // 远小于系统提示词本身的开销
	sess := compressSession(t, a, 12, 400)
	if _, err := a.CompressNow(context.Background(), sess.ID); err == nil ||
		!strings.Contains(err.Error(), "占满上下文预算") {
		t.Errorf("应报预算被占满，实际: %v", err)
	}
}
