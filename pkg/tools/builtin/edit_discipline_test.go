package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/tools"
)

// 本文件覆盖「精确替换 vs 整文件重写」这条链路：
// 读取带行号锚点与分页、多处替换原子生效、行尾符兼容、覆盖写入回传抖动统计。

func tmpFS(t *testing.T) (*FS, string) {
	t.Helper()
	root := t.TempDir()
	return NewFS(root), root
}

func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// read_file：行号锚点 / 分页
// ---------------------------------------------------------------------------

func TestReadFileLineAnchors(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "a.txt")
	writeRaw(t, path, "alpha\nbeta\ngamma")

	res, err := NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{"path": "a.txt"}))
	if err != nil || !res.Success {
		t.Fatalf("读取应成功: err=%v res=%+v", err, res)
	}
	got, _ := res.Data.(string)
	want := "     1\talpha\n     2\tbeta\n     3\tgamma\n"
	if got != want {
		t.Errorf("行号锚点格式不符\n得到: %q\n期望: %q", got, want)
	}
	if res.Metadata["total_lines"] != 3 {
		t.Errorf("metadata.total_lines 应为 3，实际 %v", res.Metadata["total_lines"])
	}
}

func TestReadFilePagesLargeFile(t *testing.T) {
	fs, root := tmpFS(t)
	const total = defaultReadLines + 500

	// 短行：行数上限先到。
	var short strings.Builder
	for i := 1; i <= total; i++ {
		short.WriteString("x\n")
	}
	writeRaw(t, filepath.Join(root, "short.txt"), short.String())

	res, _ := NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{"path": "short.txt"}))
	if got := res.Metadata["to"]; got != defaultReadLines {
		t.Fatalf("短行文件应按行数上限切窗，实际读到第 %v 行", got)
	}
	note, _ := res.Data.(string)
	if !strings.Contains(note, fmt.Sprintf("共 %d 行", total)) || !strings.Contains(note, "start_line=2001") {
		t.Errorf("未给出「还剩多少、怎么续读」的提示: %s", tailOf(note, 120))
	}

	// 长行：字节上限先到，但仍必须给出可续读的位置，且分页读遍不丢内容。
	var wide strings.Builder
	for i := 1; i <= total; i++ {
		fmt.Fprintf(&wide, "line-%d %s\n", i, strings.Repeat("abcdefghij", 8))
	}
	writeRaw(t, filepath.Join(root, "wide.txt"), wide.String())

	seen := map[int]bool{}
	for start := 1; start <= total; {
		res, _ := NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
			"path": "wide.txt", "start_line": start,
		}))
		to := res.Metadata["to"].(int)
		if to < start {
			t.Fatalf("从第 %d 行起没有前进（to=%d），续读位置提示失效", start, to)
		}
		for i := start; i <= to; i++ {
			seen[i] = true
		}
		if got := res.Data.(string); to < total && !strings.Contains(got, "未读") {
			t.Errorf("读到第 %d 行仍有一页，应提示未读满", to)
		}
		start = to + 1
	}
	if len(seen) != total {
		t.Errorf("分页读遍后应覆盖 %d 行，实际 %d 行", total, len(seen))
	}

	// 起始行越界要说清文件真实长度，而不是回一个空串让模型继续往后探。
	res, _ = NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": "wide.txt", "start_line": total + 10,
	}))
	if msg, _ := res.Data.(string); !strings.Contains(msg, fmt.Sprintf("共 %d 行", total)) {
		t.Errorf("越界读取应回报文件总行数，实际: %q", msg)
	}
}

// tailOf 取文本末尾若干字节（测试失败时只打印尾部提示，避免刷屏）。
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// 单行超长（压缩过的 JS/CSS）不得整行灌进上下文。
func TestReadFileClipsLongLine(t *testing.T) {
	fs, root := tmpFS(t)
	writeRaw(t, filepath.Join(root, "min.js"), strings.Repeat("x", maxLineRunes*3))

	res, _ := NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{"path": "min.js"}))
	got, _ := res.Data.(string)
	if len(got) > maxLineRunes+200 {
		t.Errorf("超长行未被截断，返回 %d 字节", len(got))
	}
	if !strings.Contains(got, "已截断") {
		t.Errorf("缺少超长行截断标记: %s", got[len(got)-120:])
	}
}

// 返回值必须留在执行器的输出限幅之内：被按字节硬切会让模型拿着残本重写。
func TestReadFileStaysUnderExecutorCap(t *testing.T) {
	fs, root := tmpFS(t)
	var sb strings.Builder
	for i := 0; sb.Len() < maxReadBytes*3; i++ {
		fmt.Fprintf(&sb, "%s\n", strings.Repeat("abcdefghij", 8))
		if i > defaultReadLines {
			break
		}
	}
	writeRaw(t, filepath.Join(root, "wide.txt"), sb.String())

	res, _ := NewReadFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{"path": "wide.txt"}))
	data, _ := json.Marshal(res)
	if len(data) > 32*1024 {
		t.Errorf("读取结果 %d 字节，超过执行器 32KiB 限幅会被静默截断", len(data))
	}
}

// ---------------------------------------------------------------------------
// edit_file：多处替换 / 原子性 / 行尾符
// ---------------------------------------------------------------------------

func TestEditFileMultipleReplacements(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "multi.go")
	writeRaw(t, path, "package a\n\nfunc One() { return 1 }\n\nfunc Two() { return 2 }\n\nfunc Three() { return 3 }\n")

	res, err := NewEditFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_string": "func One() { return 1 }", "new_string": "func One() { return 10 }"},
			{"old_string": "func Two() { return 2 }", "new_string": "func Two() { return 20 }"},
			{"old_string": "func Three() { return 3 }", "new_string": "func Three() { return 30 }"},
		},
	}))
	if err != nil || !res.Success {
		t.Fatalf("多处替换应成功: err=%v res=%+v", err, res)
	}
	data, _ := os.ReadFile(path)
	got := string(data)
	for _, want := range []string{"return 10", "return 20", "return 30"} {
		if !strings.Contains(got, want) {
			t.Errorf("缺少替换结果 %q，实际:\n%s", want, got)
		}
	}
	if m, _ := res.Data.(map[string]any); m["hunks"] != 3 || m["replacements"] != 3 {
		t.Errorf("结果应报告 3 处替换，实际 %+v", m)
	}
}

// 任何一处不匹配，整笔都不能落盘 —— 半套改动比不改更糟。
func TestEditFileAtomicRollback(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "atomic.txt")
	original := "one\ntwo\nthree\n"
	writeRaw(t, path, original)

	res, _ := NewEditFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path,
		"edits": []map[string]any{
			{"old_string": "one", "new_string": "ONE"},
			{"old_string": "不存在的片段", "new_string": "X"},
		},
	}))
	if res.Success {
		t.Fatal("第 2 处不匹配时整笔应失败")
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Errorf("失败后文件被改动了:\n%s", data)
	}
	if !strings.Contains(res.Error, "第 2/2 处") || !strings.Contains(res.Error, "未写入任何改动") {
		t.Errorf("错误信息应指明是第几处且未落盘，实际: %s", res.Error)
	}
}

// Windows 工作区里的 CRLF 文件：模型照抄 \n 片段也必须能替换。
func TestEditFileCRLFTolerant(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "crlf.txt")
	writeRaw(t, path, "alpha\r\nbeta\r\ngamma\r\n")

	res, _ := NewEditFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "alpha\nbeta", "new_string": "ALPHA\nBETA",
	}))
	if !res.Success {
		t.Fatalf("CRLF 文件应能按 LF 片段替换: %s", res.Error)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "ALPHA\r\nBETA\r\ngamma\r\n" {
		t.Errorf("替换后应保持 CRLF 行尾，实际: %q", data)
	}
}

// 找不到原文时给出可执行的下一步，而不是把模型推向整文件重写。
func TestEditFileNearMissHint(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "hint.txt")
	writeRaw(t, path, "l1\nl2\n    indented target\nl4\n")

	res, _ := NewEditFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "old_string": "indented target\nl999", "new_string": "x",
	}))
	if res.Success {
		t.Fatal("后续行对不上时应匹配失败")
	}
	if !strings.Contains(res.Error, "第 3 行") {
		t.Errorf("应指明首行内容所在行号，实际: %s", res.Error)
	}
}

func TestEditFileRejectsMixedArgs(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "mixed.txt")
	writeRaw(t, path, "a\n")

	res, _ := NewEditFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
		"path":       path,
		"old_string": "a",
		"new_string": "b",
		"edits":      []map[string]any{{"old_string": "a", "new_string": "b"}},
	}))
	if res.Success || !strings.Contains(res.Error, "二选一") {
		t.Errorf("edits 与顶层参数混用应被拒绝，实际: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// write_file：抖动回传 / 防误清空
// ---------------------------------------------------------------------------

func TestWriteFileReportsChurn(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "churn.txt")
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	writeRaw(t, path, sb.String())

	// 整份重写：应回传 +N/-M 并提示改用精确替换。
	rewritten := strings.ReplaceAll(sb.String(), "line ", "row ")
	res, _ := NewWriteFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "content": rewritten,
	}))
	m, _ := res.Data.(map[string]any)
	if m["added"] != 100 || m["removed"] != 100 {
		t.Fatalf("整份重写应报 +100/-100，实际 %+v", m)
	}
	if h, _ := m["hint"].(string); !strings.Contains(h, "整文件覆盖") || !strings.Contains(h, "精确替换") {
		t.Errorf("抖动过大时应提醒，实际 hint=%q", h)
	}

	// 局部追加：不该再啰嗦。
	res, _ = NewWriteFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "content": rewritten + "one more line\n",
	}))
	if m, _ := res.Data.(map[string]any); m["hint"] != nil {
		t.Errorf("新增 1 行不应触发覆盖提醒，实际: %v", m["hint"])
	}
	if m, _ := res.Data.(map[string]any); m["added"] != 1 {
		t.Errorf("应只报 1 行新增，实际 %+v", m)
	}
}

func TestWriteFileDetectsNoopOverwrite(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "noop.txt")
	writeRaw(t, path, "same\ncontent\n")

	res, _ := NewWriteFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{
		"path": path, "content": "same\ncontent\n",
	}))
	m, _ := res.Data.(map[string]any)
	if h, _ := m["hint"].(string); !strings.Contains(h, "完全一致") {
		t.Errorf("重复写入同样内容时应提示无改动，实际: %v", m["hint"])
	}
}

// 漏传 content 绝不能把已有文件静默清空。
func TestWriteFileRejectsMissingContent(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "keep.txt")
	writeRaw(t, path, "important\n")

	res, _ := NewWriteFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]string{"path": "keep.txt"}))
	if res.Success {
		t.Fatal("缺少 content 时不应执行写入")
	}
	if data, _ := os.ReadFile(path); string(data) != "important\n" {
		t.Errorf("文件被清空了: %q", data)
	}
	// 显式清空仍然允许。
	res, _ = NewWriteFileTool(fs).Execute(context.Background(), mustJSON(t, map[string]any{
		"path": path, "content": "",
	}))
	if !res.Success {
		t.Errorf("显式传空串应可清空文件: %s", res.Error)
	}
}

// ---------------------------------------------------------------------------
// 先读后写闸门
// ---------------------------------------------------------------------------

func sessionCtx(id string) context.Context {
	return tools.WithSession(context.Background(), tools.SessionScope{SessionID: id, Step: 1})
}

func TestWriteEditRequirePriorRead(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "known.txt")
	const original = "first\nsecond\nthird\n"
	writeRaw(t, path, original)
	ctx := sessionCtx("sess-A")

	res, _ := NewEditFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": path, "old_string": "second", "new_string": "SECOND",
	}))
	if res.Success || !strings.Contains(res.Error, "还没读过") {
		t.Errorf("未读过的文件应拒绝编辑，实际: %+v", res)
	}
	res, _ = NewWriteFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": path, "content": "wiped\n",
	}))
	if res.Success {
		t.Fatal("未读过的文件应拒绝覆盖写入")
	}
	if data, _ := os.ReadFile(path); string(data) != original {
		t.Errorf("被拦下的写入仍然改了文件: %q", data)
	}

	if res, _ := NewReadFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{"path": path})); !res.Success {
		t.Fatalf("读取失败: %s", res.Error)
	}
	res, _ = NewEditFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": path, "old_string": "second", "new_string": "SECOND",
	}))
	if !res.Success {
		t.Errorf("同会话读过之后应允许编辑: %s", res.Error)
	}

	// 登记按会话隔离：子智能体或另一段对话没读过，就不能顺着上一条会话的眼熟直接下笔。
	res, _ = NewWriteFileTool(fs).Execute(sessionCtx("sess-B"), mustJSON(t, map[string]string{
		"path": path, "content": "x\n",
	}))
	if res.Success {
		t.Error("别的会话读过不算读过，应仍然拒绝")
	}
}

func TestWriteFileNewFileNeedsNoRead(t *testing.T) {
	fs, root := tmpFS(t)
	ctx := sessionCtx("new")
	target := filepath.Join(root, "sub", "fresh.txt")

	res, _ := NewWriteFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": target, "content": "hello\n",
	}))
	if !res.Success {
		t.Fatalf("新建文件不该被闸门挡住: %s", res.Error)
	}
	// 自己刚写过的，接着改不必重读一遍。
	res, _ = NewEditFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": target, "old_string": "hello", "new_string": "hello world",
	}))
	if !res.Success {
		t.Errorf("本会话刚写过的文件应可直接编辑: %s", res.Error)
	}
}

// 压缩把原文挤出送模视图后，「读过」就不再是事实：登记必须作废。
func TestForgetReadsAfterCompression(t *testing.T) {
	fs, root := tmpFS(t)
	path := filepath.Join(root, "compress.txt")
	writeRaw(t, path, "a\nb\n")
	ctx := sessionCtx("compress-me")

	if res, _ := NewReadFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{"path": path})); !res.Success {
		t.Fatalf("读取失败: %s", res.Error)
	}
	if res, _ := NewEditFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": path, "old_string": "b", "new_string": "B",
	})); !res.Success {
		t.Fatalf("读过之后应可编辑: %s", res.Error)
	}

	NewWriteFileTool(fs).ForgetReads("compress-me")

	res, _ := NewEditFileTool(fs).Execute(ctx, mustJSON(t, map[string]string{
		"path": path, "old_string": "a", "new_string": "A",
	}))
	if res.Success || !strings.Contains(res.Error, "还没读过") {
		t.Errorf("作废登记后应重新要求读取，实际: %+v", res)
	}
}
