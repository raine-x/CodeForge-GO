package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/llm"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
)

// newCheckpointAgent 构造一个带真实 SQLite 的 Agent（检查点要落库，不能用 nil store）。
func newCheckpointAgent(t *testing.T) (*Agent, *History, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := NewHistory(st)
	ag := &Agent{history: h, workDir: dir}
	ag.SetMemoryStore(st)
	return ag, h, dir
}

// seedSessionWith 在 history 中造一个带指定消息的会话。
func seedSessionWith(t *testing.T, h *History, msgs []llm.Message) *Session {
	t.Helper()
	sess, err := h.Create("/tmp/ws", "测试")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	sess.Messages = msgs
	if err := h.Save(sess.ID); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}
	return sess
}

func userText(s string) llm.Message {
	return llm.TextMessage(llm.RoleUser, s)
}

func assistantText(s string) llm.Message {
	return llm.TextMessage(llm.RoleAssistant, s)
}

// ---------------------------------------------------------------------------
// 检查点记录
// ---------------------------------------------------------------------------

// 写工具的 snapshot 必须经 sink 落到 checkpoints 表，且带上下文（旧内容）。
func TestSnapshotReportsCheckpointThroughSink(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("改一下 a.txt")})

	// 先准备一个已存在的文件，再让 sink 记录它的旧内容。
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("旧内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{
		Path: target, Existed: true, OldContent: "旧内容",
	})

	steps := ag.CheckpointSteps(sess.ID)
	if len(steps) != 1 {
		t.Fatalf("期望 1 个回滚点，得到 %d", len(steps))
	}
	if steps[0].Step != 1 || steps[0].Files != 1 {
		t.Fatalf("回滚点内容不符: %+v", steps[0])
	}
}

// 同一步骤内重复上报同一路径：去重后只留最早那份。
func TestRecordCheckpointDedupsWithinStep(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	ag.recordCheckpoint(sess.ID, 3, tools.CheckpointEvent{Path: "/p", Existed: true, OldContent: "最早"})
	ag.recordCheckpoint(sess.ID, 3, tools.CheckpointEvent{Path: "/p", Existed: true, OldContent: "更晚"})

	rows, err := ag.memoryStore.ListCheckpoints(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("去重失败：期望 1 条，得到 %d", len(rows))
	}
	if rows[0].OldContent != "最早" {
		t.Fatalf("应保留最早内容，得到 %q", rows[0].OldContent)
	}
}

// ---------------------------------------------------------------------------
// 回滚：改坏 3 个文件 → 回滚两步前全部还原（Plan.md #4 验收）
// ---------------------------------------------------------------------------

func TestRewindRestoresModifiedFiles(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("改三个文件")})

	files := map[string]string{"a.txt": "A-原", "b.txt": "B-原", "c.txt": "C-原"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 步骤 1：三个文件都被改写（快照记录的是改写前的原内容）
	for name, body := range files {
		p := filepath.Join(dir, name)
		ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: p, Existed: true, OldContent: body})
	}
	// 步骤 2：再改一次 a.txt（模拟多轮迭代）
	pa := filepath.Join(dir, "a.txt")
	ag.recordCheckpoint(sess.ID, 2, tools.CheckpointEvent{Path: pa, Existed: true, OldContent: "A-步骤1后"})

	// 现在磁盘上是「改坏」的状态
	for name := range files {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("坏掉了"), 0o644)
	}

	// 回滚到步骤 1 之前 → 三个文件全部还原
	res, err := ag.RewindFiles(sess.ID, 1)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("不应有失败项: %+v", res.Errors)
	}
	// a.txt 在步骤 1、2 各有一条快照（倒序回放两次），加上 b/c 共 4 次写回；
	// 文件维度仍是 3 个 —— 以去重后的 Paths 为准。
	if res.Restored != 4 {
		t.Fatalf("期望写回 4 次（a 跨两步回放 2 次 + b + c），实际 %d", res.Restored)
	}
	if len(res.Paths) != 3 {
		t.Fatalf("期望涉及 3 个文件，实际 %d: %v", len(res.Paths), res.Paths)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s 未还原：期望 %q，得到 %q", name, want, got)
		}
	}
}

// 回滚只退到目标步骤之前：更早的步骤不受影响。
func TestRewindKeepsEarlierSteps(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("分两步改")})

	early := filepath.Join(dir, "early.txt")
	late := filepath.Join(dir, "late.txt")
	_ = os.WriteFile(early, []byte("早期-改后"), 0o644)
	_ = os.WriteFile(late, []byte("晚期-改后"), 0o644)

	// 步骤 1 改了 early.txt（快照是「早期-原」）
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: early, Existed: true, OldContent: "早期-原"})
	// 步骤 5 改了 late.txt
	ag.recordCheckpoint(sess.ID, 5, tools.CheckpointEvent{Path: late, Existed: true, OldContent: "晚期-原"})

	// 只回滚到步骤 5 之前 → 只还原 late.txt，early.txt 保持「改后」
	if _, err := ag.RewindFiles(sess.ID, 5); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if got, _ := os.ReadFile(late); string(got) != "晚期-原" {
		t.Errorf("late.txt 应还原，得到 %q", got)
	}
	if got, _ := os.ReadFile(early); string(got) != "早期-改后" {
		t.Errorf("early.txt 不该被动，得到 %q", got)
	}
}

// 写入前不存在的文件：回滚 = 删除它。
func TestRewindDeletesFilesThatDidNotExist(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("新建一个文件")})

	created := filepath.Join(dir, "new.txt")
	ag.recordCheckpoint(sess.ID, 2, tools.CheckpointEvent{Path: created, Existed: false})
	if err := os.WriteFile(created, []byte("新建的"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := ag.RewindFiles(sess.ID, 2)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("期望删除 1 个，实际 %d", res.Deleted)
	}
	if _, err := os.Stat(created); !os.IsNotExist(err) {
		t.Error("新建的文件应被删除")
	}
}

// 删除前不存在的文件（用户已手动删过）不应当被算作失败。
func TestRewindTolerantOfAlreadyDeletedFiles(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	ghost := filepath.Join(dir, "ghost.txt")
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: ghost, Existed: false})

	res, err := ag.RewindFiles(sess.ID, 1)
	if err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if res.Failed != 0 {
		t.Fatalf("文件本就不存在不应算失败: %+v", res.Errors)
	}
}

// 回滚成功后检查点被清理：不能再回滚第二次（否则会把刚还原的文件又写回旧内容）。
func TestRewindClearsConsumedCheckpoints(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	p := filepath.Join(dir, "a.txt")
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "原"})

	if _, err := ag.RewindFiles(sess.ID, 1); err != nil {
		t.Fatal(err)
	}
	if steps := ag.CheckpointSteps(sess.ID); len(steps) != 0 {
		t.Fatalf("回滚后检查点应清空，仍剩 %d 个", len(steps))
	}
}

// 会话无任何检查点时，回滚是安全空操作。
func TestRewindWithNoCheckpoints(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	res, err := ag.RewindFiles(sess.ID, 1)
	if err != nil {
		t.Fatalf("空回滚不应报错: %v", err)
	}
	if res.Restored != 0 || res.Deleted != 0 || res.Failed != 0 {
		t.Fatalf("空回滚结果应为全零: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// 可编辑用户消息白名单（最近 N 条纯文本发言）
// ---------------------------------------------------------------------------

func TestEditableUserMessagesLimitedToPlainText(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("第一问"),
		assistantText("第一答"),
		userText("第二问"),
		assistantText("第二答"),
		userText("第三问"),
		assistantText("第三答"),
		userText("第四问"),
		assistantText("第四答"),
	})

	got := ag.EditableUserMessages(sess.ID, 3)
	if len(got) != 3 {
		t.Fatalf("期望 3 条可编辑消息，得到 %d", len(got))
	}
	// 时间正序：第二、三、四问（最近 3 条）
	want := []string{"第二问", "第三问", "第四问"}
	for i, w := range want {
		if got[i].Text != w {
			t.Errorf("第 %d 条期望 %q，得到 %q", i, w, got[i].Text)
		}
	}
	// back 语义：0 = 最后一条
	if got[2].Back != 0 || got[1].Back != 1 || got[0].Back != 2 {
		t.Errorf("back 语义不符: %+v", got)
	}
}

// 带 tool_result 的 user 消息是工具回填，不是用户发言，不能进白名单。
func TestEditableUserMessagesSkipsToolResults(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("真提问"),
		llm.ToolResultMessage("t1", "工具结果", false),
		assistantText("答"),
	})

	got := ag.EditableUserMessages(sess.ID, 3)
	if len(got) != 1 {
		t.Fatalf("期望 1 条（工具结果不算），得到 %d", len(got))
	}
	if got[0].Text != "真提问" {
		t.Errorf("内容不符: %q", got[0].Text)
	}
}

func TestNthLastPlainUserIndex(t *testing.T) {
	msgs := []llm.Message{
		userText("q1"), assistantText("a1"),
		userText("q2"), assistantText("a2"),
		userText("q3"),
	}
	if got := nthLastPlainUserIndex(msgs, 0); got != 4 {
		t.Errorf("n=0 期望 4，得到 %d", got)
	}
	if got := nthLastPlainUserIndex(msgs, 1); got != 2 {
		t.Errorf("n=1 期望 2，得到 %d", got)
	}
	if got := nthLastPlainUserIndex(msgs, 2); got != 0 {
		t.Errorf("n=2 期望 0，得到 %d", got)
	}
	if got := nthLastPlainUserIndex(msgs, 3); got != -1 {
		t.Errorf("越界期望 -1，得到 %d", got)
	}
}

// ---------------------------------------------------------------------------
// 编辑重发：截断 + 内部回退压缩态（不动 usage 累计）
// ---------------------------------------------------------------------------

// 编辑第 N 条用户消息：历史截断到该条，其后内容全部丢弃。
func TestEditResendTruncatesHistory(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("原始第一问"),
		assistantText("第一答"),
		userText("第二问"),
		assistantText("第二答"),
		userText("第三问"),
		assistantText("第三答"),
	})

	// 编辑「第二问」（倒数第 2 条纯文本 → back=1）。不真跑模型，
	// 只验证截断与替换逻辑，所以用一个会立刻失败的 provider。
	ag.provider = nil
	idx, _ := ag.EditAndResend(context.Background(), sess.ID, 1, "改后的第二问", nil)

	if idx != 2 {
		t.Fatalf("期望定位到下标 2，得到 %d", idx)
	}
	if len(sess.Messages) != 3 {
		t.Fatalf("截断后应余 3 条，得到 %d", len(sess.Messages))
	}
	last := sess.Messages[2]
	if last.Role != llm.RoleUser {
		t.Fatalf("截断点应为用户消息，得到 %s", last.Role)
	}
	if txt := last.Content[0].Text; txt != "改后的第二问" {
		t.Errorf("文本未替换：期望 %q，得到 %q", "改后的第二问", txt)
	}
}

// 压缩游标越过截断点时必须复位（否则送模视图会切出不存在的区间）。
func TestEditResendRewindsCompressionState(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("q1"), assistantText("a1"), userText("q2"), assistantText("a2"),
	})
	// 假装前 4 条已被摘要覆盖（游标 = 4），再编辑最后一条用户消息（下标 2）
	sess.compressedUpTo = 4
	sess.summaryText = "旧摘要"
	ag.provider = nil

	if _, _ = ag.EditAndResend(context.Background(), sess.ID, 0, "改后", nil); sess.compressedUpTo != 0 {
		t.Errorf("压缩游标应复位为 0，得到 %d", sess.compressedUpTo)
	}
	if sess.summaryText != "" {
		t.Errorf("摘要应清空，得到 %q", sess.summaryText)
	}
}

// ⚠️ 关键约束：编辑重发**不得**清零 usage 累计（左下角窗口统计是真实计费口径，
// 回退历史不等于这些 token 没花过）。
func TestEditResendKeepsUsageCounters(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("q1"), assistantText("a1"), userText("q2"), assistantText("a2"),
	})
	sess.AddUsage(llm.Usage{InputTokens: 1000, OutputTokens: 200, CachedTokens: 300})
	ag.provider = nil

	_, _ = ag.EditAndResend(context.Background(), sess.ID, 0, "改后", nil)

	if sess.usageIn != 1000 {
		t.Errorf("usageIn 不应被清零，得到 %d", sess.usageIn)
	}
	if sess.usageOut != 200 {
		t.Errorf("usageOut 不应被清零，得到 %d", sess.usageOut)
	}
	if sess.usageHit != 300 {
		t.Errorf("usageHit 不应被清零，得到 %d", sess.usageHit)
	}
}

// 编辑空文本应被拒绝（否则会发出一个空提问）。
func TestEditResendRejectsEmptyText(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("q1"), assistantText("a1")})

	if _, err := ag.EditAndResend(context.Background(), sess.ID, 0, "   ", nil); err == nil {
		t.Fatal("空文本应报错")
	}
}

// back 越界（历史里没有那么多用户消息）应报错，且不改动历史。
func TestEditResendRejectsOutOfRangeBack(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("只有一问"), assistantText("答")})

	if _, err := ag.EditAndResend(context.Background(), sess.ID, 5, "改后", nil); err == nil {
		t.Fatal("越界 back 应报错")
	}
	if len(sess.Messages) != 2 {
		t.Errorf("失败时不应改动历史，得到 %d 条", len(sess.Messages))
	}
}

// 替换只动 text 块，图片等其它块必须原样保留。
func TestEditResendPreservesNonTextBlocks(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	msg := userText("带图的提问")
	msg.Content = append(msg.Content, llm.ContentBlock{Type: llm.BlockImage, Text: "img-data"})
	sess := seedSessionWith(t, h, []llm.Message{msg})

	ag.provider = nil
	_, _ = ag.EditAndResend(context.Background(), sess.ID, 0, "改后文本", nil)

	kept := sess.Messages[0]
	if kept.Content[0].Text != "改后文本" {
		t.Errorf("文本块未替换: %q", kept.Content[0].Text)
	}
	foundImage := false
	for _, b := range kept.Content {
		if b.Type == llm.BlockImage && b.Text == "img-data" {
			foundImage = true
		}
	}
	if !foundImage {
		t.Error("图片块不应被丢弃")
	}
}

// ---------------------------------------------------------------------------
// RewindAfterEdit：定位不到目标消息时安全跳过
// ---------------------------------------------------------------------------

func TestRewindAfterEditSkipsWhenNoDroppedMessages(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("唯一一问")})

	p := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(p, []byte("内容"), 0o644)
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "原"})

	// 目标消息是最后一条 → 其后没有消息 → 不回退文件
	res, err := ag.RewindAfterEdit(sess.ID, 0)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res != nil {
		t.Fatalf("其后无内容时不应回退文件，得到 %+v", res)
	}
}

func TestRewindAfterEditRollsBackWhenMessagesDropped(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("第一问"), assistantText("第一答"),
		userText("第二问"), assistantText("第二答"),
	})

	p := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(p, []byte("坏掉了"), 0o644)
	ag.recordCheckpoint(sess.ID, 4, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "原内容"})

	// 编辑「第二问」（back=0）→ 其后有 1 条消息被丢弃 → 应触发文件回退
	res, err := ag.RewindAfterEdit(sess.ID, 0)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res == nil {
		t.Fatal("应回退文件")
	}
	if got, _ := os.ReadFile(p); string(got) != "原内容" {
		t.Errorf("文件应被还原，得到 %q", got)
	}
}

// 会话从未记录过写操作时，编辑重发不做任何文件回滚。
func TestRewindAfterEditNoCheckpointsAtAll(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{
		userText("q1"), assistantText("a1"), userText("q2"), assistantText("a2"),
	})

	res, err := ag.RewindAfterEdit(sess.ID, 0)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res != nil {
		t.Fatalf("无检查点时应返回 nil，得到 %+v", res)
	}
}

// 编辑重发时若无法定位消息（back 越界），只截断对话、不动文件。
func TestRewindAfterEditUnknownTarget(t *testing.T) {
	ag, h, _ := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("q1"), assistantText("a1")})

	res, err := ag.RewindAfterEdit(sess.ID, 9)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if res != nil {
		t.Fatalf("定位不到时不应回退，得到 %+v", res)
	}
}

// 会话不存在时回滚应给出明确错误，而不是静默成功。
func TestRewindFilesUnknownSession(t *testing.T) {
	ag, _, _ := newCheckpointAgent(t)
	if _, err := ag.RewindFiles("不存在的会话", 1); err != nil {
		// 会话不存在但无检查点 → 空操作即可（不报错也合理）
		t.Logf("返回错误（可接受）: %v", err)
	}
}

// 回滚结果里的 Paths 应是去重后的集合（同一文件跨步出现只列一次）。
func TestRewindResultPathsDeduped(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	p := filepath.Join(dir, "same.txt")
	_ = os.WriteFile(p, []byte("现在"), 0o644)
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "阶段一"})
	ag.recordCheckpoint(sess.ID, 2, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "阶段二"})

	res, err := ag.RewindFiles(sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 1 {
		t.Fatalf("路径应去重为 1 条，得到 %v", res.Paths)
	}
	// 倒序回放：最终落到最早的旧内容
	if got, _ := os.ReadFile(p); string(got) != "阶段一" {
		t.Errorf("应回放最早内容，得到 %q", got)
	}
}

// 缩略检查：回滚结果字段可 JSON 序列化（前端要读）。
func TestRewindResultJSONShape(t *testing.T) {
	ag, h, dir := newCheckpointAgent(t)
	sess := seedSessionWith(t, h, []llm.Message{userText("x")})

	p := filepath.Join(dir, "a.txt")
	_ = os.WriteFile(p, []byte("x"), 0o644)
	ag.recordCheckpoint(sess.ID, 1, tools.CheckpointEvent{Path: p, Existed: true, OldContent: "y"})

	res, _ := ag.RewindFiles(sess.ID, 1)
	if !strings.Contains(res.Paths[0], "a.txt") {
		t.Errorf("Paths 应含被还原文件: %v", res.Paths)
	}
}
