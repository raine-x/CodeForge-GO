package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codeforge/pkg/tools"
)

// 本文件覆盖 2.7 的核心目标：**同一工作区内多个会话并行时不互相干扰**。
//
// 改造前的四个真实故障（撤销栈与阅读登记都是工作区级的）：
//
//	1. A 点「撤销」撤掉的是 B 的写入
//	2. A 读过的文件让 B 以为「这个会话读过它」，B 可以凭记忆改写
//	3. A 的撤销把 B 的文件改回去
//	4. 预算逐出时 A 的大文件挤掉 B 的快照

// writeIn 在指定会话下写一个文件（走真实工具链：CAS + 快照 + 指纹）。
func writeIn(t *testing.T, fs *FS, sessionID, path, content string) {
	t.Helper()
	readIn(t, fs, sessionID, path)
	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: sessionID})
	res, err := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path":    path,
		"content": content,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("会话 %s 写入失败: %+v", sessionID, res)
	}
}

// readIn 通过 read_file 工具读一次（用于「登记阅读」这个副作用）。
func readIn(t *testing.T, fs *FS, sessionID, path string) {
	t.Helper()
	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: sessionID})
	if _, err := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path": path,
	})); err != nil {
		t.Fatal(err)
	}
}

// TestUndoDepthIsPerSession 撤销深度必须按会话算。
//
// 这是「A 撤销撤掉 B 的改动」的第一道关口：HTTP 撤销接口回填的
// remaining 若是全局深度，A 点一次之后看到「还剩 12 步可撤销」，
// 而其中 12 步全是 B 的 —— 数字本身就是错的。
func TestUndoDepthIsPerSession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	writeIn(t, fs, "A", filepath.Join(dir, "a.txt"), "A1")
	writeIn(t, fs, "A", filepath.Join(dir, "a2.txt"), "A2")
	writeIn(t, fs, "B", filepath.Join(dir, "b.txt"), "B1")

	if got := fs.UndoDepthFor("A"); got != 2 {
		t.Errorf("会话 A 的撤销深度应为 2（只数自己的），实际 %d", got)
	}
	if got := fs.UndoDepthFor("B"); got != 1 {
		t.Errorf("会话 B 的撤销深度应为 1，实际 %d", got)
	}
	// 未知会话应为 0，而不是把别人的算给它
	if got := fs.UndoDepthFor("C"); got != 0 {
		t.Errorf("未写入过的会话深度应为 0，实际 %d", got)
	}
}

// TestUndoSessionNeverTouchesOtherSessionFiles 核心用例。
//
// 改造前 undo 是**一条线性栈**，`UndoSession` 只是「从后往前找第一条匹配」，
// 而 `Undo()`（HTTP 接口走它）直接弹栈顶 —— A 点撤销就撤掉 B。
func TestUndoSessionNeverTouchesOtherSessionFiles(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	fileA := filepath.Join(dir, "a.txt")
	fileB := filepath.Join(dir, "b.txt")

	writeIn(t, fs, "A", fileA, "A-content")
	writeIn(t, fs, "B", fileB, "B-content")

	// A 撤销自己的
	if _, ok := fs.UndoSession("A"); !ok {
		t.Fatalf("A 应能撤销自己的写入")
	}
	// B 的文件必须纹丝不动
	if got, err := os.ReadFile(fileB); err != nil {
		t.Fatalf("B 的文件不该被 A 的撤销影响: %v", err)
	} else if string(got) != "B-content" {
		t.Errorf("B 的文件被 A 的撤销改了：%q", got)
	}
	// B 仍能撤销自己的
	if _, ok := fs.UndoSession("B"); !ok {
		t.Errorf("B 的快照应仍在，A 的撤销不该消耗掉它")
	}
}

// TestReadSeenIsolatedPerSession 指纹必须按会话隔离。
//
// 这是故障 2：A 读过的文件，B 不该因此获得「凭记忆改写」的资格。
// 否则 B 可以在完全没看过内容的情况下覆盖 A 的改动，且守卫层认为正常。
func TestReadSeenIsolatedPerSession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	p := filepath.Join(dir, "shared.txt")

	// A 读过
	writeIn(t, fs, "A", p, "v1")

	// B 没读过，直接写 → 必须被拒
	ctxB := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: "B"})
	res, err := NewWriteFileTool(fs).Execute(ctxB, mustArgs(t, map[string]any{
		"path":    p,
		"content": "B 凭记忆改写",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Success {
		t.Errorf("B 没读过该文件却写成功了 —— 指纹被跨会话串用了。" +
			"A 读过的登记不应让 B 获得改写资格")
	}
}

// TestReadSeenSurvivesOtherSessionUndo 撤销不应误伤别人的指纹。
//
// 故障 3：A 撤销后，B 对该文件的指纹必须仍然有效（或者被作废），
// 但绝不能「因为 A 撤销了所以 B 的指纹被改成 A 的旧值」——
// 那会让 B 下次写入基于一个错误的基线通过。
func TestReadSeenSurvivesOtherSessionUndo(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	p := filepath.Join(dir, "shared.txt")

	// 预置文件，使两次写入都是「覆盖已有文件」。
	// 否则 A 的第一次写是新建，撤销 = 删文件，后面就读不到了 ——
	// 那样测的就不是「指纹是否被误改」，而是「文件还在不在」。
	if err := os.WriteFile(p, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeIn(t, fs, "A", p, "v1") // A 读+写
	writeIn(t, fs, "B", p, "v2") // B 读+写

	// A 撤销 → 文件回到 A 写之前的内容，即预置的 v0
	// （快照存的是**写前**内容，不是 A 写进去的 v1 —— 这点容易搞反）
	if _, ok := fs.UndoSession("A"); !ok {
		t.Fatalf("A 撤销应成功")
	}

	// 关键：B 的指纹必须已被刷新到 v0。
	// 撤销会改文件内容，而 CAS 的指纹比对的是「当前内容 vs 登记的指纹」——
	// 若这里漏刷，B 下次写入就会基于 v2 的指纹去比对 v0 的文件，
	// 被误判成「被外部修改」而失败，且报错完全指错方向。
	if got := readFileRaw(t, p); got != "v0" {
		t.Fatalf("A 撤销后文件应回到 v0，实际 %q", got)
	}
	// B **不重读**直接写：指纹已被撤销刷新，应该成功。
	// 这一步是本用例的核心 —— 若撤销漏刷指纹，B 的指纹仍指向 v2，
	// 而文件已是 v0，CAS 会判成「被外部修改」而拒绝。
	writeWithoutRead(t, fs, "B", p, "v3")
	if got := readFileRaw(t, p); got != "v3" {
		t.Errorf("B 再次写入后应是 v3，实际 %q", got)
	}
}

// writeWithoutRead 在不重新读取的前提下写入（依赖已有的指纹登记）。
func writeWithoutRead(t *testing.T, fs *FS, sessionID, path, content string) {
	t.Helper()
	ctx := tools.WithSession(context.Background(),
		tools.SessionScope{SessionID: sessionID})
	res, err := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path":    path,
		"content": content,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.Success {
		t.Fatalf("会话 %s 在未重读的情况下写入被拒: %+v", sessionID, res)
	}
}

// readFileRaw 直接读文件原始内容。
func readFileRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}

// TestBudgetEvictionPrefersOtherSessions 预算逐出不能只砍别人的。
//
// 故障 4：A 写了一个大文件把预算撑爆，逐出逻辑若从头部丢，
// 丢的可能是 B 更早的快照。合理策略是：**优先逐出超出预算的
// 那一条自己**（它就是肇事者），而不是随机/从头部砍。
//
// 这条测试只断言「B 的快照在被 A 撑爆预算后仍然可撤销」。
func TestBudgetEvictionPrefersOtherSessions(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	// maxCount=2：B 写两条，然后 A 写一条撑爆
	fs.SetUndoLimits(2, 0, 0, 0)

	fileB1 := filepath.Join(dir, "b1.txt")
	fileB2 := filepath.Join(dir, "b2.txt")
	writeIn(t, fs, "B", fileB1, "B1")
	writeIn(t, fs, "B", fileB2, "B2")

	// A 连写 4 条，触发逐出
	for i := 0; i < 4; i++ {
		p := filepath.Join(dir, "a.txt")
		writeIn(t, fs, "A", p, "A"+string(rune('0'+i)))
	}

	// B 至少还剩一条能撤
	depth := fs.UndoDepthFor("B")
	if depth == 0 {
		t.Errorf("A 撑爆预算后，B 的快照应至少还剩一条，实际 0 条")
	}
	if _, ok := fs.UndoSession("B"); !ok {
		t.Errorf("A 撑爆预算后 B 仍应能撤销自己的写入")
	}
}

// TestSnapshotsForSession 分桶后 Snapshots 要能按会话取。
func TestSnapshotsForSession(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	writeIn(t, fs, "A", filepath.Join(dir, "a1.txt"), "A1")
	writeIn(t, fs, "B", filepath.Join(dir, "b1.txt"), "B1")
	writeIn(t, fs, "A", filepath.Join(dir, "a2.txt"), "A2")

	as := fs.SnapshotsFor("A")
	if len(as) != 2 {
		t.Fatalf("A 的快照应为 2 条，实际 %d", len(as))
	}
	for _, s := range as {
		if s.SessionID != "A" {
			t.Errorf("A 的快照里混进了会话 %q 的条目", s.SessionID)
		}
	}
	bs := fs.SnapshotsFor("B")
	if len(bs) != 1 {
		t.Fatalf("B 的快照应为 1 条，实际 %d", len(bs))
	}
}

// TestConcurrentSessionsUndoIndependently 并行撤销互不干扰。
func TestConcurrentSessionsUndoIndependently(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)

	sessions := []string{"A", "B", "C", "D"}
	files := map[string]string{}
	for _, s := range sessions {
		files[s] = filepath.Join(dir, s+".txt")
		writeIn(t, fs, s, files[s], s+"-v1")
	}

	// 各会话各撤一次，应全部成功且路径归自己
	for _, s := range sessions {
		p, ok := fs.UndoSession(s)
		if !ok {
			t.Fatalf("会话 %s 撤销失败", s)
		}
		if filepath.Base(p) != filepath.Base(files[s]) {
			t.Errorf("会话 %s 撤销拿到了 %s", s, filepath.Base(p))
		}
	}
	// 全部撤完
	for _, s := range sessions {
		if got := fs.UndoDepthFor(s); got != 0 {
			t.Errorf("会话 %s 应已撤空，实际还剩 %d", s, got)
		}
	}
}
