package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"codeforge/config"
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

// ---------------------------------------------------------------------------
// 热更新字段与运行中循环的并发（2026-09-23）
//
// 设置页的四类改动走 HTTP 处理器 goroutine：应用模型（SetProvider +
// SetLLMConfig）、切工作区（SetWorkDir）、切内置插件开关（Set*Enabled），
// 而运行中的循环在同一个进程里读它们（provider.Stream / 请求参数 /
// System Prompt 组装 / 技能与记忆路径）。
//
// 其中 builtinOn 是 map：裸读写撞上就是
// `fatal error: concurrent map read and map write`，整个进程直接死、无法 recover。
// ---------------------------------------------------------------------------

// hotUpdateProvider 每轮都返回一段文本，让循环快速走完一步。
func hotUpdateProvider() llm.Provider {
	return maxStepsProvider(func(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
		return okStream("答"), nil
	})
}

// 内置插件开关：读侧（System Prompt 组装）与写侧（设置页）直接对撞。
//
// 这是上面那条的**确定性**版本：读者是紧循环的真实读路径（systemPromptFor
// 里就会走到 builtinOnSnapshot），写者是紧循环的开关切换。刻意不加任何
// 额外负载，让两侧的迭代频率都足够高 —— 反向验证过：去掉 setBuiltinOn 的锁，
// 本用例稳定崩在 `fatal error: concurrent map iteration and map write`
//（栈顶就是 builtinOnSnapshot ← builtinPluginSection ← systemPrompt）。
func TestBuiltinPluginToggleWhileReading(t *testing.T) {
	a := newEmitTestAgent(t, hotUpdateProvider())
	sess, err := a.History().Create("", "插件开关")
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.systemPromptFor(sess)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			a.SetSkillCreatorEnabled(true)
			a.SetMultiAgentEnabled(true)
			a.SetPlanEnabled(true)
		}
	}()

	// 跑一小会儿真实循环，让「正在跑的任务」这条路径也一起被压。
	_ = a.Run(context.Background(), sess.ID, "跑一轮", func(Event) {})
	close(stop)
	wg.Wait()
}

// 热更新与运行并发时不得崩：四个字段全部走锁 + 快照。
//
// ⚠️ 本机没有 C 编译器（-race 依赖 cgo），这是确定性压力用例而非竞态检测器：
// 它能把「裸 map 并发读写」这类必然致命的问题逼出来（反向验证过：把
// setBuiltinOn 的锁去掉，本用例稳定崩在 concurrent map read and map write），
// 但证明不了「没有任何数据竞争」。
//
// 结构是「多个紧循环读者 + 一个紧循环写者」而不是「跑几轮 Run」：
// 单轮 Run 只组装一次 System Prompt，读窗口太窄，压不出并发（实测踩过这个坑）。
func TestHotUpdateWhileRunning(t *testing.T) {
	a := newEmitTestAgent(t, hotUpdateProvider())
	sess, err := a.History().Create("", "热更新")
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// 读者：运行中的循环每一步都要组装 System Prompt（读 builtinOn / workDir），
	// 每发一次请求还要取 llmCfg 与 provider。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.systemPromptFor(sess)
				_ = a.llmCfgSnapshot()
				_ = a.providerSnapshot()
			}
		}()
	}

	// 写者：模拟设置页的四类改动（应用模型 / 切工作区 / 切插件开关）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			a.SetLLMConfig(config.LLMConfig{MaxTokens: 4096, Temperature: 0.7})
			a.SetProvider(hotUpdateProvider())
			a.SetWorkDir("/tmp/热更新")
			a.SetSkillCreatorEnabled(true)
			a.SetMultiAgentEnabled(true)
			a.SetPlanEnabled(true)
			_ = a.WorkDir()
			_ = a.builtinOnSnapshot()
		}
	}()

	// 顺带跑几轮真实循环：热更新撞上「正在跑的任务」是最典型的场景。
	for i := 0; i < 5; i++ {
		if err := a.Run(context.Background(), sess.ID, "跑一轮", func(Event) {}); err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("第 %d 轮失败: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

// 快照语义：设置页改了之后，读者必须看到**整份**新值，而不是新旧混合。
func TestHotUpdateSnapshotIsConsistent(t *testing.T) {
	a := &Agent{}
	a.SetLLMConfig(config.LLMConfig{MaxTokens: 1000, Temperature: 0.1})
	if got := a.llmCfgSnapshot(); got.MaxTokens != 1000 || got.Temperature != 0.1 {
		t.Fatalf("快照应为整份配置，实际 %+v", got)
	}

	// 未设置的字段要有稳定默认，不能让调用方拿到半份配置。
	a.SetWorkDir("/tmp/ws")
	if got := a.WorkDir(); got != "/tmp/ws" {
		t.Fatalf("WorkDir 应为 /tmp/ws，实际 %q", got)
	}
	if got := a.builtinOnSnapshot(); len(got) != 0 {
		t.Fatalf("未开启任何插件时应为空表，实际 %+v", got)
	}
	a.SetPlanEnabled(true)
	// 快照是副本：改内部表不得影响已发出的快照。
	snap := a.builtinOnSnapshot()
	a.SetPlanEnabled(false)
	if !snap[BuiltinPlan.ID] {
		t.Fatal("快照必须是副本，不能随源表变化")
	}
	if a.builtinOnSnapshot()[BuiltinPlan.ID] {
		t.Fatal("关掉后快照应为 false")
	}
}
