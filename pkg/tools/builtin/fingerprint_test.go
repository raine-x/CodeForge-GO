package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/tools"
)

// 这组测试针对「先读后写」的 TOCTOU 缺口：
// 改造前 readSeen 是 map[string]bool，**只记「读过」不记「读的是哪一版」**，
// 于是这个序列会被静默放行：
//
//	1. Agent 读 A（内容 v1）
//	2. 外部程序改动 A（内容 v2）
//	3. Agent 基于 v1 的理解写 A
//
// 结果是 Agent 覆盖掉 v2 的改动，而守卫层认为一切正常 ——
// 数据丢失且无任何报错。

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustArgs(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newFSTest(t *testing.T) (*FS, string) {
	t.Helper()
	dir := t.TempDir()
	return NewFS(dir), dir
}

// TestWriteBlockedAfterExternalChange 核心用例：读到之后被外部改过，写入必须被拒。
func TestWriteBlockedAfterExternalChange(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	rf := NewReadFileTool(fs)
	wf := NewWriteFileTool(fs)

	if r, _ := rf.Execute(ctx, mustArgs(t, map[string]string{"path": p})); r == nil || !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	// 外部改动 —— 模拟另一个进程 / 用户 / 另一个工具
	writeFile(t, p, "v2")

	r, _ := wf.Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "agent 的写法"}))
	if r == nil {
		t.Fatal("不应返回 nil")
	}
	if r.Success {
		t.Fatalf("文件被外部改过后，写入必须被拒；实际成功了")
	}
	// 内容必须仍是外部那版
	got, _ := os.ReadFile(p)
	if string(got) != "v2" {
		t.Errorf("外部的修改被覆盖了：得到 %q，期望 %q", string(got), "v2")
	}
}

// TestWriteBlockedAfterExternalEdit 同上，走 edit_file 路径。
//
// 构造上有个坑：外部改动**不能**把待替换的 old_string 删掉，否则 applyPairs
// 会先失败（"找不到匹配"），根本没走到读门禁 —— 那样这个用例是假通过。
// 所以让外部改动去**追加**一行：待替换的文本仍在，替换本身会成功，
// 没有指纹校验时就会把外部那行悄悄丢掉。
func TestWriteBlockedAfterExternalEdit(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "line1\nline2\n")

	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	writeFile(t, p, "line1\nline2\n别人加的一行\n") // 外部追加

	r, _ := NewEditFileTool(fs).Execute(ctx, mustArgs(t, map[string]any{
		"path":  p,
		"edits": []map[string]any{{"old_string": "line2", "new_string": "agent"}},
	}))
	if r.Success {
		t.Fatalf("被外部改过后 edit_file 必须被拒（否则会悄悄丢弃外部的新增）")
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "别人加的一行") {
		t.Errorf("外部新增的内容被丢弃了：%q", string(got))
	}
}

// TestWriteStillAllowedNormally 未被外部改动时必须放行 —— 不能因为加了校验就误伤。
func TestWriteStillAllowedNormally(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatalf("未被外部改动，写入应放行: %+v", r)
	}
}

// TestWriteTwiceInARow 是「写后刷新指纹」这个坑的核心用例。
//
// 如果写成功后不更新指纹，会出现：读 A → 写 A → 再写 A，第二次因
// 「指纹 v1 ≠ 实际 v2」被拒 —— **Agent 被自己的写操作锁死**，
// 而且报错是「文件被外部修改过」，极具误导性。
func TestWriteTwiceInARow(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	rf, wf := NewReadFileTool(fs), NewWriteFileTool(fs)
	if r, _ := rf.Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := wf.Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatalf("第一次写应成功: %+v", r)
	}
	// ↓ 这一步在「写后不刷新指纹」的实现下必然失败
	if r, _ := wf.Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v3"})); !r.Success {
		t.Fatalf("连续第二次写必须成功（写后应刷新指纹）: %+v", r)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "v3" {
		t.Errorf("内容应是 v3，实际 %q", string(got))
	}
}

// TestEditTwiceInARow 同上，走 edit_file。
func TestEditTwiceInARow(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "one\ntwo\n")

	rf, ef := NewReadFileTool(fs), NewEditFileTool(fs)
	if r, _ := rf.Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	edit := func(old, nw string) *tools.ToolResult {
		r, _ := ef.Execute(ctx, mustArgs(t, map[string]any{
			"path": p, "edits": []map[string]any{{"old_string": old, "new_string": nw}},
		}))
		return r
	}
	if r := edit("one", "ONE"); !r.Success {
		t.Fatalf("第一次改应成功: %+v", r)
	}
	if r := edit("two", "TWO"); !r.Success {
		t.Fatalf("连续第二次改必须成功: %+v", r)
	}
}

// TestUndoRefreshesFingerprint 撤销会改回文件内容，指纹同样要刷新，
// 否则「读 → 写 → 撤销 → 写」会在最后一步被拒。
func TestUndoRefreshesFingerprint(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	rf, wf := NewReadFileTool(fs), NewWriteFileTool(fs)
	if r, _ := rf.Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	if r, _ := wf.Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v2"})); !r.Success {
		t.Fatalf("写应成功: %+v", r)
	}
	if _, ok := fs.Undo(); !ok {
		t.Fatal("撤销栈应非空")
	}
	if r, _ := wf.Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "v3"})); !r.Success {
		t.Fatalf("撤销后再写必须成功: %+v", r)
	}
}

// TestReadGateStillEnforcesNotRead 未读就写仍然被拒 —— 指纹校验不能顶替原有的门槛。
func TestReadGateStillEnforcesNotRead(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "x"}))
	if r.Success {
		t.Fatalf("没读过就写必须被拒")
	}
}

// TestFingerprintClearedOnCompress 压缩后作废阅读登记时，指纹也必须一起清，
// 否则压缩后模型看到的是摘要却仍握有旧指纹，约束被悄悄放宽。
func TestFingerprintClearedOnCompress(t *testing.T) {
	fs, dir := newFSTest(t)
	ctx := sessionCtx("s1")
	p := filepath.Join(dir, "a.txt")
	writeFile(t, p, "v1")

	if r, _ := NewReadFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p})); !r.Success {
		t.Fatalf("读应成功: %+v", r)
	}
	fs.ForgetReads("s1")

	r, _ := NewWriteFileTool(fs).Execute(ctx, mustArgs(t, map[string]string{"path": p, "content": "x"}))
	if r.Success {
		t.Fatalf("压缩作废后应回到「本会话没读过」而被拒")
	}
}
