package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"codeforge/config"
	"codeforge/pkg/security"
	"codeforge/pkg/tools"
)

// TestLoadSessionSwitchesWorkspaceToSessionOwn 钉住「点开某项目的会话，
// 工作区必须切到那个会话自己的工作区」。
//
// 用户报告的现象：RaineOS 项目里点开一条会话让它分析，结果它去分析了
// 另一个项目（test）的文件。
//
// 根因：会话表里本来就存着每条会话自己的工作区（sessions.workspace），
// 但 load_session 帧只回放历史消息，从不读它。于是文件工具用的仍是
// 进程当前全局的 FS.root —— 也就是「界面上上次选的那个项目」。
//
// 后果不止「读到别的项目」：
//   - 撤销栈、阅读登记（readSeen）都是工作区级的，会跨项目混用；
//   - 一个项目的快照里记着另一个项目的文件路径，撤销时写错地方；
//   - 审计日志里「谁读了哪个项目」这条线断了。
func TestLoadSessionSwitchesWorkspaceToSessionOwn(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectRaine := t.TempDir()
	projectTest := t.TempDir()

	sessRaine, err := d.agent.History().Create(projectRaine, "分析 RaineOS")
	if err != nil {
		t.Fatalf("建 RaineOS 会话失败: %v", err)
	}
	if sessRaine.Workspace != projectRaine {
		t.Fatalf("会话应记住自己的工作区 %q，实际 %q", projectRaine, sessRaine.Workspace)
	}

	// 当前全局工作区在 test 项目（模拟用户之前选了它）
	d.fsys.SetRoot(projectTest)
	d.agent.SetWorkDir(projectTest)

	sess, ok := d.agent.History().Get(sessRaine.ID)
	if !ok {
		t.Fatalf("取会话失败")
	}
	srv.switchSessionWorkspace(sess)

	if got := d.fsys.Root(); got != projectRaine {
		t.Errorf("点开 RaineOS 的会话后，工作区应是 %q，实际 %q —— "+
			"Agent 会去读/写另一个项目的文件", projectRaine, got)
	}
	if got := d.agent.WorkDir(); got != projectRaine {
		t.Errorf("Agent 的 workDir 应同步切到 %q，实际 %q", projectRaine, got)
	}
}

// TestSwitchSessionBackAndForthIsStable 来回切换必须稳定。
//
// 只测一次不够：真实使用是 A→B→A 反复切。
// 若某次切换漏了某个步骤（只切了 FS 没切 Agent，或反之），第二次往返会暴露。
func TestSwitchSessionBackAndForthIsStable(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectA := t.TempDir()
	projectB := t.TempDir()

	sessA, _ := d.agent.History().Create(projectA, "A")
	sessB, _ := d.agent.History().Create(projectB, "B")

	for round := 0; round < 5; round++ {
		s, _ := d.agent.History().Get(sessA.ID)
		srv.switchSessionWorkspace(s)
		assertBothAt(t, d, projectA, "第 "+strconv.Itoa(round)+" 轮切到 A")

		s, _ = d.agent.History().Get(sessB.ID)
		srv.switchSessionWorkspace(s)
		assertBothAt(t, d, projectB, "第 "+strconv.Itoa(round)+" 轮切到 B")
	}
}

// assertBothAt 断言 FS 与 Agent 的工作区都在同一处。
//
// 两个都要查：它们是两份独立状态（builtin.FS.root 与 Agent.workDir），
// 只同步一个是历史上常见的一半修法。
func assertBothAt(t *testing.T, d *testDeps, want, when string) {
	t.Helper()
	if got := d.fsys.Root(); got != want {
		t.Errorf("%s: FS.root 应为 %q，实际 %q", when, want, got)
	}
	if got := d.agent.WorkDir(); got != want {
		t.Errorf("%s: Agent.workDir 应为 %q，实际 %q", when, want, got)
	}
}

// TestSwitchToSessionWithEmptyWorkspaceIsNoop 边界：会话没绑定工作区
// （老数据、或手工建的会话）时不应把当前工作区清空。
func TestSwitchToSessionWithEmptyWorkspaceIsNoop(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	project := t.TempDir()
	d.fsys.SetRoot(project)
	d.agent.SetWorkDir(project)

	sess, _ := d.agent.History().Create("", "无工作区会话")
	if sess.Workspace != "" {
		t.Fatalf("前置条件失败：会话应无工作区，实际 %q", sess.Workspace)
	}

	srv.switchSessionWorkspace(sess)
	if got := d.fsys.Root(); got != project {
		t.Errorf("会话无工作区时应保持当前工作区 %q 不变，实际 %q", project, got)
	}
	if got := d.agent.WorkDir(); got != project {
		t.Errorf("Agent.workDir 也应保持 %q，实际 %q", project, got)
	}
}

// TestSwitchToSessionWithDeletedWorkspaceIsNoop 边界：会话绑定的工作区
// 已被删除/移动时保持现状 —— 报「目录不存在」比静默切到错的项目安全。
func TestSwitchToSessionWithDeletedWorkspaceIsNoop(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	project := t.TempDir()
	d.fsys.SetRoot(project)
	d.agent.SetWorkDir(project)

	gone := t.TempDir()
	sess, _ := d.agent.History().Create(gone, "工作区已删")
	// 把目录删掉，模拟项目被移动/重命名
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("删除目录失败: %v", err)
	}

	srv.switchSessionWorkspace(sess)
	if got := d.fsys.Root(); got != project {
		t.Errorf("会话工作区已不存在时应保持 %q 不变，实际 %q", project, got)
	}
}

// TestSwitchingWorkspaceClearsCrossProjectState 切换必须清掉跨项目状态。
//
// 撤销栈与阅读登记是**工作区级**的。切项目不清的话：
//   - 在 B 项目点「撤销」，会撤掉 A 项目的写入；
//   - A 项目里的 readSeen 会让 B 会话以为「这个会话读过该文件」。
//
// 两者都是静默的数据错误。
func TestSwitchingWorkspaceClearsCrossProjectState(t *testing.T) {
	d := newTestDeps(t)
	srv := New(d.cfg, d.agent, d.executor, d.registry, d.fsys)

	projectA := t.TempDir()
	projectB := t.TempDir()
	fileInA := filepath.Join(projectA, "a.txt")

	// 在 A 项目里写一个文件，压一条撤销栈
	d.fsys.SetRoot(projectA)
	d.agent.SetWorkDir(projectA)
	pressUndoSnapshot(t, d, fileInA)

	if d.fsys.UndoDepth() == 0 {
		t.Fatalf("前置条件失败：应有一条撤销快照")
	}

	// 切到 B 项目
	sessB, _ := d.agent.History().Create(projectB, "B")
	s, _ := d.agent.History().Get(sessB.ID)
	srv.switchSessionWorkspace(s)

	if got := d.fsys.UndoDepth(); got != 0 {
		t.Errorf("切工作区后撤销栈应清空（栈里是另一个项目的路径），实际还剩 %d 条", got)
	}
}

// pressUndoSnapshot 用一个「写类工具全 allow」的 executor 写入，
// 从而压出一条撤销快照。
//
// 为什么不用 d.executor：它在 newTestDeps 里按 default.yaml 的规则建好，
// 而那里 write_file 是 ask、approver 是 nil（无审批通道 → 直接失败）。
// 本项要验的是「切工作区清不清跨项目状态」，绕开审批反而更纯粹。
func pressUndoSnapshot(t *testing.T, d *testDeps, path string) {
	t.Helper()
	audit, err := security.NewAuditLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })

	policy := security.NewPolicy(config.SecurityConfig{
		DefaultDecision: "allow",
		Rules: []config.SecurityRule{{
			Tools:    []string{"write_file", "edit_file", "read_file", "delete_file"},
			Decision: "allow",
		}},
	})
	ex := tools.NewExecutor(d.registry, policy, audit, nil, 30*time.Second, 32*1024)

	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: "s1"})
	js := func(m map[string]any) json.RawMessage {
		b, _ := json.Marshal(m)
		return b
	}
	if _, err := ex.Execute(ctx, "read_file", js(map[string]any{"path": path})); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	res, err := ex.Execute(ctx, "write_file", js(map[string]any{
		"path":    path,
		"content": "AAA",
	}))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if res == nil || !res.Success {
		t.Fatalf("写入应成功: %+v", res)
	}
}
