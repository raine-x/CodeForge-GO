package server

import (
	"context"
	"testing"
	"time"

	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

// textProvider 返回一句正常的助手回复，一轮循环会正常收尾。
type textProvider struct{}

func (*textProvider) Name() string { return "text-stub" }

func (*textProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	ch := make(chan llm.StreamEvent, 2)
	ch <- llm.StreamEvent{Type: llm.EventTextDelta, Text: "好的"}
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
}

// blockingProvider 第一次 Stream 一直阻塞，直到 release 被关掉。
// 用来让 Agent 的循环停在「等模型响应」这一步，运行态持续挂着。
type blockingProvider struct {
	release chan struct{}
	once    bool
}

func (*blockingProvider) Name() string { return "blocking-stub" }

func (p *blockingProvider) Stream(context.Context, llm.Request) (<-chan llm.StreamEvent, error) {
	if !p.once {
		p.once = true
		<-p.release
	}
	ch := make(chan llm.StreamEvent, 1)
	ch <- llm.StreamEvent{Type: llm.EventMessageStop}
	close(ch)
	return ch, nil
}

// runOneTurn 让会话真的跑一轮（用 idleProvider，秒级收尾），
// 用于验证「运行结束后切换被放行」。
func runOneTurn(t *testing.T, d *testDeps, sessionID string) {
	t.Helper()
	// Run 要求非 nil 的 Emitter（生产路径由 WS 层提供）；这里只要一个丢弃实现。
	emit := func(agent.Event) {}
	if err := d.agent.Run(context.Background(), sessionID, "看一下这个项目", emit); err != nil {
		t.Fatalf("跑一轮失败: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.agent.IsRunning(sessionID) {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		return
	}
	t.Fatalf("一轮跑完后运行态仍未清除")
}

// TestSwitchWorkspaceAllowedAfterRunFinishes 跑完一轮后必须还能切。
//
// 这是对上一条「运行中拒绝切换」的必要制衡：
// 拒绝一旦收得太紧（比如忘了在 endRun 时放开），用户就永远切不动工作区，
// 而界面上看不出任何原因。
func TestSwitchWorkspaceAllowedAfterRunFinishes(t *testing.T) {
	d := newTestDepsAtProvider(t, "", &textProvider{})
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectA := t.TempDir()
	projectB := t.TempDir()

	sessA, _ := d.agent.History().Create(projectA, "A")
	d.fsys.SetRoot(projectA)
	d.agent.SetWorkDir(projectA)

	runOneTurn(t, d, sessA.ID)
	if d.agent.IsRunning(sessA.ID) {
		t.Fatalf("前置条件失败：一轮结束后不应仍在运行")
	}

	sessB, _ := d.agent.History().Create(projectB, "B")
	bs, _ := d.agent.History().Get(sessB.ID)
	if !srv.switchSessionWorkspace(bs) {
		t.Fatalf("运行结束后应允许切换工作区")
	}
	if got := d.fsys.Root(); got != projectB {
		t.Errorf("工作区应切到 %q，实际 %q", projectB, got)
	}
}

// TestSwitchWorkspaceRefusesWhileAnotherSessionRunning 运行中的会话不得被切走。
//
// 回归：切会话会调 switchSessionWorkspace → FS.SetRoot。
// 而 FS.root 是**进程全局**的，那次切换会连带改掉正在跑的那个会话
// 所看到的工作区。后果比「读到别的项目」更糟：
//
//	会话 A 正在分析项目 A，用户顺手点开会话 B（项目 B）
//	  → FS.root 被改成项目 B
//	  → A 的下一轮工具调用（read_file / search_files…）落到项目 B 里
//	  → A 的分析结果混进 B 的文件，且 A 自己毫无察觉
//
// 「A 正在跑、用户顺手点了别的会话」在界面上是很自然的操作，不是边缘用法。
// 所以运行中必须拒绝切换，而不是硬切。
func TestSwitchWorkspaceRefusesWhileAnotherSessionRunning(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectA := t.TempDir()
	projectB := t.TempDir()

	sessA, _ := d.agent.History().Create(projectA, "分析中")
	d.fsys.SetRoot(projectA)
	d.agent.SetWorkDir(projectA)

	// 让 A 停在运行态
	release := markRunning(t, d, sessA.ID)
	defer release()

	sessB, _ := d.agent.History().Create(projectB, "另一个项目")
	bs, _ := d.agent.History().Get(sessB.ID)

	if srv.switchSessionWorkspace(bs) {
		t.Errorf("会话 A 正在运行时，不应允许把工作区切到会话 B 的项目")
	}
	if got := d.fsys.Root(); got != projectA {
		t.Errorf("工作区应保持在运行中会话 A 的项目 %q，实际 %q", projectA, got)
	}
	if got := d.agent.WorkDir(); got != projectA {
		t.Errorf("Agent.workDir 应保持在 %q，实际 %q", projectA, got)
	}
}

// TestSwitchWorkspaceAllowsSwitchToRunningSelf 点开自己正在跑的会话要放行。
//
// 这不是「切走」—— 工作区本来就该是它的，不该被自己的运行态挡住。
func TestSwitchWorkspaceAllowsSwitchToRunningSelf(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectA := t.TempDir()
	sessA, _ := d.agent.History().Create(projectA, "A")
	d.fsys.SetRoot(projectA)
	d.agent.SetWorkDir(projectA)
	release := markRunning(t, d, sessA.ID)
	defer release()

	as, _ := d.agent.History().Get(sessA.ID)
	if !srv.switchSessionWorkspace(as) {
		t.Errorf("点开自己正在跑的会话应放行（工作区无需变动）")
	}
	if got := d.fsys.Root(); got != projectA {
		t.Errorf("工作区应保持 %q，实际 %q", projectA, got)
	}
}

// TestSameWorkspaceSessionsSwitchFreelyWhileRunning 同一工作区里，
// 一个会话在跑时**另一个会话必须仍能点开** —— 这就是并发。
//
// 这条是被用户实际撞到后加的：曾经 switchSessionWorkspace 里有一条
// 「任何会话在跑就拒绝切换」，于是同一项目里第二个会话根本点不开。
// 而同工作区的两个会话压根不需要切 FS.root（开头就 return true），
// 那条限制纯属误伤。
func TestSameWorkspaceSessionsSwitchFreelyWhileRunning(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	project := t.TempDir()
	sessA, _ := d.agent.History().Create(project, "A")
	sessB, _ := d.agent.History().Create(project, "B")

	d.fsys.SetRoot(project)
	d.agent.SetWorkDir(project)
	release := markRunning(t, d, sessA.ID)
	defer release()

	// A 在跑，切到 B —— 必须成功（同一工作区，不用切）
	bs, _ := d.agent.History().Get(sessB.ID)
	if !srv.switchSessionWorkspace(bs) {
		t.Fatalf("同一项目里，A 在跑时切到 B 应被允许 —— 这正是并发的前提")
	}
	if got := d.fsys.Root(); got != project {
		t.Errorf("工作区应保持 %q，实际 %q", project, got)
	}
	if got := d.agent.WorkDir(); got != project {
		t.Errorf("Agent.workDir 应保持 %q，实际 %q", project, got)
	}
	// 再切回 A 也应成功
	as, _ := d.agent.History().Get(sessA.ID)
	if !srv.switchSessionWorkspace(as) {
		t.Errorf("切回 A 应被允许")
	}
}

// markRunning 让会话真的停在运行态。
//
// 用一个**阻塞**的 provider：Agent 跑到「等模型响应」那一步就停住，
// 运行态一直挂着，直到 release 被关掉。这样测的是真实路径，
// 不用为了测试去扩大 Agent 的公开 API。
func markRunning(t *testing.T, d *testDeps, sessionID string) (release func()) {
	t.Helper()
	p := &blockingProvider{release: make(chan struct{})}
	d.agent.SetProvider(p)

	emit := func(agent.Event) {}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.agent.Run(context.Background(), sessionID, "分析一下", emit)
	}()

	// 等它真的进入运行态
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if d.agent.IsRunning(sessionID) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !d.agent.IsRunning(sessionID) {
		t.Fatalf("会话未能在超时内进入运行态")
	}
	return func() {
		close(p.release)
		<-done
	}
}
