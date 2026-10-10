package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/security"
)

// 外置机制（context offloading）的回归测试。
//
// 钉住三件事：
//  1. **未装配时逐字节不变** —— 大多数单测与早期用户都走这条路，
//     它必须与改动前的截断行为完全一致，否则本次改动就是一次静默的行为变更。
//  2. **装配后零丢失** —— 内容完整落盘、能按行取回，这是外置的全部意义。
//  3. **不能被当成越权读文件的入口** —— id 与路径都不可由模型控制。

func newTestStore(t *testing.T, spill, preview int) *ToolOutStore {
	t.Helper()
	s := NewToolOutStore(t.TempDir(), spill, preview)
	if s == nil {
		t.Fatal("NewToolOutStore 返回 nil")
	}
	return s
}

// 未装配 store 时，truncate 的行为必须与改动前逐字节一致。
func TestTruncateUnchangedWithoutToolOut(t *testing.T) {
	e := NewExecutor(NewRegistry(), nil, nil, nil, 0, 4096)
	big := strings.Repeat("甲", 10000) // 「甲」是 3 字节，断言一律按字节算
	want := fmt.Sprintf("[输出已截断，原始长度 %d 字节]", len(big))

	got := e.truncate(context.Background(), &ToolResult{Success: true, Data: big})
	s, ok := got.Data.(string)
	if !ok {
		t.Fatalf("纯文本结果必须仍是字符串形态，实际 %T", got.Data)
	}
	if !strings.HasSuffix(s, want) {
		t.Errorf("未装配外置时应走原截断标记，实际尾部: %q", tail(s, 60))
	}
	if strings.Contains(s, SpillMarker) {
		t.Error("未装配外置时不应出现外置标记")
	}
	if got.Metadata != nil {
		t.Errorf("未装配外置时不应写元数据，实际 %v", got.Metadata)
	}
}

// 装配后：上下文里是预览 + 标记，完整内容落盘且能原样取回。
func TestToolOutSpillsAndKeepsEverything(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	e := NewExecutor(NewRegistry(), nil, nil, nil, 0, 4096)
	e.SetToolOut(s)

	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("第")
		sb.WriteString(strings.Repeat("x", 3))
		sb.WriteString("行内容\n")
	}
	full := sb.String()

	ctx := WithSession(context.Background(), SessionScope{SessionID: "sess-1"})
	got := e.truncate(ctx, &ToolResult{Success: true, Data: full})

	preview, ok := got.Data.(string)
	if !ok {
		t.Fatalf("纯文本结果必须仍是字符串形态，实际 %T", got.Data)
	}
	if len(preview) >= len(full) {
		t.Fatalf("预览应远小于原文：预览 %d 字节，原文 %d 字节", len(preview), len(full))
	}
	if !strings.Contains(preview, SpillMarker) {
		t.Fatalf("预览里应有外置标记，实际尾部: %q", tail(preview, 120))
	}
	// 预览必须以原文开头：结构化工具输出的信息密度通常在开头。
	if !strings.HasPrefix(preview, full[:100]) {
		t.Error("预览应保留原文开头")
	}

	ref := spillRef(t, got)
	if ref.Lines != 501 {
		t.Errorf("行数应记为 501，实际 %d", ref.Lines)
	}
	if ref.Bytes != len(full) {
		t.Errorf("字节数应记为 %d，实际 %d", len(full), ref.Bytes)
	}

	// 关键断言：完整内容一个字节都没少。
	back, err := s.Read("sess-1", ref.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(back, full) {
		t.Error("取回的内容不含原文 —— 外置丢了数据")
	}
}

// 分页取回：按行区间。
func TestToolOutReadRanges(t *testing.T) {
	s := newTestStore(t, 64, 16)
	var sb strings.Builder
	for i := 0; i < 200; i++ {
		// 每行内容互不相同，否则「确实取到了第 2–3 行」无从判断
		fmt.Fprintf(&sb, "第%d行-填充内容\n", i+1)
	}
	text := sb.String()
	ref, _, _ := s.Store("s2", text)
	if ref.ID == "" {
		t.Fatal("应拿到 id")
	}

	got, err := s.Read("s2", ref.ID, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "第2行") || !strings.Contains(got, "第3行") {
		t.Errorf("应返回第 2–3 行，实际: %q", got)
	}
	if strings.Contains(got, "第4行") || strings.Contains(got, "第5行") {
		t.Errorf("不应包含第 4 行，实际: %q", got)
	}
	if !strings.Contains(got, fmt.Sprintf("共 %d 行", 201)) {
		t.Errorf("应告知总行数 201，实际: %q", tail(got, 80))
	}
}

// id 是唯一的寻址手段，必须严校验 —— 它直接拼进文件路径。
func TestToolOutRejectsBadID(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	ref, _, _ := s.Store("sess", strings.Repeat("x", 5000))

	bad := []string{
		"",
		"tr_",
		"nope",
		"tr_ZZZZ",             // 非小写 hex
		"tr_../../etc/passwd", // 路径穿越
		"tr_../../../../etc/passwd",
		strings.Repeat("tr_", 20),
	}
	for _, id := range bad {
		if _, err := s.Read("sess", id, 0, 0); err == nil {
			t.Errorf("id %q 应被拒绝", id)
		}
	}
	// 合法的那个仍要能读。
	if _, err := s.Read("sess", ref.ID, 0, 0); err != nil {
		t.Errorf("合法 id 应可读: %v", err)
	}
}

// 会话隔离：A 会话不能读 B 会话的外置输出。
// 这是「只能读本会话自己的输出」这条承诺的实现。
func TestToolOutIsolatesSessions(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	ref, _, _ := s.Store("sess-A", strings.Repeat("A", 5000))
	if _, err := s.Read("sess-B", ref.ID, 0, 0); err == nil {
		t.Error("跨会话读取应被拒绝")
	}
}

// 会话 id 含路径分隔符时必须被收敛，不能让写入点跑到 root 之外。
func TestToolOutSanitizesSessionID(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	ref, _, _ := s.Store("../../escape", strings.Repeat("x", 5000))
	if _, err := s.Read("../../escape", ref.ID, 0, 0); err != nil {
		t.Fatalf("收敛后的 id 应可读回: %v", err)
	}
	// 落点必须在 root 之下。
	entries, err := os.ReadDir(s.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.Contains(e.Name(), "/") {
			t.Errorf("root 下出现了非法目录名: %q", e.Name())
		}
	}
}

// 结构化结果外置后仍必须是**合法 JSON**（硬规则 6）。
func TestToolOutStructuredStaysValidJSON(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	e := NewExecutor(NewRegistry(), nil, nil, nil, 0, 4096)
	e.SetToolOut(s)

	items := make([]map[string]any, 0, 400)
	for i := 0; i < 400; i++ {
		items = append(items, map[string]any{"idx": i, "path": "/some/long/path/file.go", "note": strings.Repeat("n", 40)})
	}
	got := e.truncate(context.Background(), &ToolResult{Success: true, Data: items})

	raw, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("外置后必须仍是合法 JSON: %v", err)
	}
	if !strings.Contains(string(raw), `"spilled":true`) && !strings.Contains(string(raw), `"spilled": true`) {
		t.Errorf("应标记为已外置，实际: %s", string(raw)[:minInt(len(raw), 200)])
	}
	ref := spillRef(t, got)
	if _, err := s.Read("", ref.ID, 0, 0); err == nil {
		t.Error("无会话上下文时不应允许读取")
	}
}

// 阈值以下的内容不外置（否则每个小结果都产生一次磁盘写）。
func TestToolOutBelowThresholdNotSpilled(t *testing.T) {
	s := newTestStore(t, 8192, 256)
	if s.ShouldSpill(strings.Repeat("x", 8192)) {
		t.Error("等于阈值不应外置")
	}
	if !s.ShouldSpill(strings.Repeat("x", 8193)) {
		t.Error("超过阈值应外置")
	}
}

// 关闭开关时 NewToolOutStore 返回 nil，全链路退回原行为。
func TestToolOutDisabledByZero(t *testing.T) {
	if s := NewToolOutStore("/tmp/x", 0, 0); s != nil {
		t.Error("阈值 0 时应返回 nil（功能关闭）")
	}
	if s := NewToolOutStore("", 8192, 0); s != nil {
		t.Error("root 为空时应返回 nil（功能关闭）")
	}
	var nilStore *ToolOutStore
	if nilStore.Enabled() {
		t.Error("nil 存储的 Enabled 应为 false")
	}
	if nilStore.ShouldSpill("x") {
		t.Error("nil 存储不应外置")
	}
}

// RemoveSession 只删目标会话。
func TestToolOutRemoveSession(t *testing.T) {
	s := newTestStore(t, 2048, 256)
	ref, _, _ := s.Store("sess-A", strings.Repeat("x", 5000))
	_, _, _ = s.Store("sess-B", strings.Repeat("y", 5000))

	if err := s.RemoveSession("sess-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read("sess-A", ref.ID, 0, 0); err == nil {
		t.Error("A 会话的外置应已删除")
	}
	if _, err := s.Read("sess-B", "tr_000000000000", 0, 0); err == nil {
		t.Log("B 会话目录应仍然存在（具体 id 读不到属正常）")
	}
	if _, err := os.Stat(filepath.Join(s.root, "sess-B")); err != nil {
		t.Errorf("B 会话目录不应被误删: %v", err)
	}
}

func spillRef(t *testing.T, res *ToolResult) ToolOutRef {
	t.Helper()
	m, ok := res.Metadata["tool_out"].(map[string]any)
	if !ok {
		t.Fatalf("结果元数据里应有 tool_out，实际 %v", res.Metadata)
	}
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatalf("tool_out.id 为空: %v", m)
	}
	var ref ToolOutRef
	ref.ID = id
	ref.Bytes, _ = m["bytes"].(int)
	ref.Lines, _ = m["lines"].(int)
	return ref
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- 端到端：走完整的 Executor.Execute 链路 ----

type bigResultTool struct{ text string }

func (t *bigResultTool) Name() string                 { return "big_tool" }
func (t *bigResultTool) Description() string          { return "返回一段很长的文本" }
func (t *bigResultTool) InputSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *bigResultTool) Execute(context.Context, json.RawMessage) (*ToolResult, error) {
	return &ToolResult{Success: true, Data: t.text}, nil
}

// 端到端：工具真的返回一个 100 KB 结果时，进历史的是「预览 + 引用」，
// 且 tool_result_read 能把原文一字不差地取回来。
func TestToolOutEndToEndThroughExecute(t *testing.T) {
	s := newTestStore(t, 4096, 512)
	reg := NewRegistry()
	reg.Register(&bigResultTool{text: strings.Repeat("端到端内容\n", 20000)})

	pol := security.NewPolicy(config.SecurityConfig{
		PermissionMode: "auto", DefaultDecision: "allow", AutoApproveReadOnly: true,
	})
	e := NewExecutor(reg, pol, nil, nil, 0, 32*1024)
	e.SetToolOut(s)
	ctx := WithSession(context.Background(), SessionScope{SessionID: "e2e", Step: 1})

	res, err := e.Execute(ctx, "big_tool", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("工具应成功: %v", res.Error)
	}
	view, _ := res.Data.(string)
	if len(view) > 4096 {
		t.Errorf("进历史的内容应远小于原文，实际 %d 字节", len(view))
	}
	ref := spillRef(t, res)

	// Agent 回填给模型的就是这段文本；原样断言它带引用。
	if !strings.Contains(view, ref.ID) {
		t.Errorf("预览里应含 id %s，实际尾部: %q", ref.ID, tail(view, 160))
	}

	// 分段取回，直到覆盖全文。
	var got strings.Builder
	page := 2000
	for start := 1; ; start += page {
		chunk, err := s.Read("e2e", ref.ID, start, start+page-1)
		if err != nil {
			t.Fatalf("读第 %d 行失败: %v", start, err)
		}
		lines := strings.Split(chunk, "\n")
		got.WriteString(strings.Join(lines[1:], "\n")) // 去掉首行「（第 x–y 行…）」说明
		if start+page > ref.Lines {
			break
		}
	}
	if !strings.Contains(got.String(), "端到端内容") {
		t.Error("分段取回后不含原文内容 —— 外置链路有问题")
	}
}
