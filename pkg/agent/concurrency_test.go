package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codeforge/pkg/llm"
)

// ---------------------------------------------------------------------------
// 同会话运行权互斥（2026-09-23）
//
// 背景：WS 的 c.stop() 只能停**本连接**的旧任务，两个浏览器标签页可以同时
// 驱动同一会话。没有互斥时，两个 ReAct 循环并发 append 同一个
// Session.Messages 并各自全量覆盖写库（Save = DELETE + 全量重插），
// 历史直接错乱（docs/修改.md 记录的 cancel-不-join 隐患）。
// ---------------------------------------------------------------------------

// gateProvider 第一轮卡在闸门前，直到测试放行 —— 模拟「上一轮还在跑」。
type gateProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *gateProvider) Name() string { return "gate-stub" }

func (p *gateProvider) Stream(ctx context.Context, _ llm.Request) (<-chan llm.StreamEvent, error) {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return okStream("好"), nil
}

// 同一会话已有循环在跑时，后到者拿不到运行权（等不到就报错，且不污染历史）。
func TestRunRejectsConcurrentSameSession(t *testing.T) {
	gate := &gateProvider{entered: make(chan struct{}), release: make(chan struct{})}
	a := newEmitTestAgent(t, gate)
	sess, err := a.History().Create("", "race")
	if err != nil {
		t.Fatal(err)
	}

	firstDone := make(chan error, 1)
	go func() { firstDone <- a.Run(context.Background(), sess.ID, "第一轮", func(Event) {}) }()
	<-gate.entered // 确认第一轮已经跑起来

	// 第二轮并发进入：必须在超时后如实报错，而不是与第一轮并存。
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := a.Run(ctx, sess.ID, "第二轮", func(Event) {}); err == nil {
		t.Fatal("同一会话并发的第二轮必须被拒绝")
	}
	// 被拒绝的轮次不得把用户消息写进历史（否则平白多出一条没人回答的提问）。
	if _, msgs := sess.SnapshotForRender(); len(msgs) != 1 {
		t.Fatalf("被拒轮次不应写入历史，实际 %d 条", len(msgs))
	}

	close(gate.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("第一轮放行后应正常完成: %v", err)
	}
	// 让位之后同一会话可以接着跑。
	if err := a.Run(context.Background(), sess.ID, "第三轮", func(Event) {}); err != nil {
		t.Fatalf("让位后应能继续: %v", err)
	}
	if _, msgs := sess.SnapshotForRender(); len(msgs) != 4 {
		t.Fatalf("三轮后应为 4 条消息，实际 %d", len(msgs))
	}
}

// beginRunWait 的让位语义：前一轮不退出就等（不抢），退出后接续。
func TestBeginRunWaitWaitsForHandoff(t *testing.T) {
	a := &Agent{}
	a.beginRun("s1")

	done := make(chan error, 1)
	go func() { done <- a.beginRunWait(context.Background(), "s1") }()

	// 确认它在等，而不是直接通过或立即报错。
	select {
	case err := <-done:
		t.Fatalf("前一轮未退出时不应拿到运行权: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	a.endRun("s1")
	if err := <-done; err != nil {
		t.Fatalf("让位后应能拿到运行权: %v", err)
	}
	a.endRun("s1")
}

// 调用方 ctx 先结束时要如实返回，不能傻等到 15 秒上限。
func TestBeginRunWaitRespectsCallerContext(t *testing.T) {
	a := &Agent{}
	a.beginRun("s1")
	defer a.endRun("s1")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := a.beginRunWait(ctx, "s1"); err == nil {
		t.Fatal("拿不到运行权且 ctx 结束时应返回错误")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("应在 ctx 结束时快速返回，实际等了 %v", elapsed)
	}
}

// ---------------------------------------------------------------------------
// 步骤号会话级单调（与检查点对齐的前提）
// ---------------------------------------------------------------------------

// 步骤号不得每轮从 1 重计：第二轮的第 1 步 = 2。否则后续轮回的同号检查点
// 会被 (会话,步骤,路径) 主键的 INSERT OR IGNORE 静默丢弃。
func TestStepNumbersAreSessionMonotonic(t *testing.T) {
	a := newEmitTestAgent(t, maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return okStream("答"), nil
	}))
	sess, err := a.History().Create("", "steps")
	if err != nil {
		t.Fatal(err)
	}

	var steps []int
	emit := func(ev Event) {
		if ev.Type == EventStep {
			steps = append(steps, ev.Step)
		}
	}
	if err := a.Run(context.Background(), sess.ID, "第一问", emit); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), sess.ID, "第二问", emit); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 || steps[0] != 1 || steps[1] != 2 {
		t.Fatalf("步骤号应会话级单调递增，实际 %v", steps)
	}
}

// ---------------------------------------------------------------------------
// RewindAfterEdit 的精确步骤映射（替代 (dropped+1)/2 近似）
// ---------------------------------------------------------------------------

// 历史里夹着 steer 注入的用户消息（不占步骤）：编辑 steer 消息应只回退
// 其后的那一步，不能把更早的步骤也带进去（旧近似口径会多退）。
func TestRewindAfterEditExactStepMapping(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	// q1 → 步骤1（改 a.txt）→ steer 插话（用户消息，不占步骤）→ 步骤2（改 b.txt）
	sess := seedSessionWith(t, h, []llm.Message{
		userText("q1"),
		assistantText("改 a"),
		llm.ToolResultMessage("t1", "ok", false),
		userText("steer：顺便改 b"),
		assistantText("改 b"),
		llm.ToolResultMessage("t2", "ok", false),
	})
	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(fileA, []byte("A-新"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("B-新"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.RecordCheckpoint(sess.ID, 1, fileA, true, "A-原")
	ag.RecordCheckpoint(sess.ID, 2, fileB, true, "B-原")

	// 编辑最近一条用户消息（steer 插话，back=0）：只回退其后的步骤 2。
	res, err := ag.RewindAfterEdit(sess.ID, 0)
	if err != nil || res == nil {
		t.Fatalf("回退失败: res=%+v err=%v", res, err)
	}
	if got, _ := os.ReadFile(fileA); string(got) != "A-新" {
		t.Errorf("步骤 1 不应被带回退区间，a.txt 应为「A-新」，实际 %q", got)
	}
	if got, _ := os.ReadFile(fileB); string(got) != "B-原" {
		t.Errorf("步骤 2 应被回退，b.txt 应为「B-原」，实际 %q", got)
	}

	// 再编辑 q1（back=1）：回退从步骤 1 起 —— a.txt 也还原。
	if _, err := ag.RewindAfterEdit(sess.ID, 1); err != nil {
		t.Fatalf("第二次回退失败: %v", err)
	}
	if got, _ := os.ReadFile(fileA); string(got) != "A-原" {
		t.Errorf("步骤 1 应被回退，a.txt 应为「A-原」，实际 %q", got)
	}
}

// ---------------------------------------------------------------------------
// History 缓存并发烟测
//
// 没有锁的时候，map 并发读写是进程级 fatal；本机无 C 编译器跑不了 -race，
// 用确定性压力用例至少保证这些路径都被并发踩过（见 docs/修改.md 的约定）。
// ---------------------------------------------------------------------------

func TestHistoryConcurrentCacheOps(t *testing.T) {
	_, h, _ := newCheckpointAgent(t)
	sess, err := h.Create("", "并发")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, ok := h.Get(sess.ID); !ok {
					t.Error("已建会话应能取到")
				}
				_ = h.Save(sess.ID)
				_ = h.Rename(sess.ID, "改名")
				_ = h.SetWorkspace(sess.ID, "/tmp/ws2")
				tmp, err := h.Create("", "临时")
				if err == nil {
					_ = h.Delete(tmp.ID)
				}
				_ = h.Archive(sess.ID)
				_ = h.Unarchive(sess.ID)
			}
		}()
	}
	wg.Wait()

	if _, ok := h.Get(sess.ID); !ok {
		t.Error("并发操作后会话应仍可读取")
	}
}
