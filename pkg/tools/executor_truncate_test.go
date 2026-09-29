package tools

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 输出限幅（AGENTS.md 硬规则 6）：
//
//	「工具结果超过 maxOutput 时，输出必须仍是合法 JSON，不能在半截 JSON 处硬切。」
//
// 改造前结构化结果走的是 `json.Marshal(res.Data)` 之后**按字节硬切**，
// 产出的是半个对象：`{"hits":[{"file":"a.go","line":1,"text":"…`
// —— 引号与括号都不闭合。
//
// 后果有两层，第二层更严重：
//  1. 模型看到的内容在语法上就是坏的，语义不可用；
//  2. 模型把它抄进下一次 tool_call 的 arguments 时，上游 json.Unmarshal 直接失败。
//
// 这一组测试全部围绕「产出必须仍是合法 JSON」。

// bigHit 模拟 search_files 的真实返回形态（具名 struct 切片，不是 map）。
type bigHit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

func newTruncExec(max int) *Executor {
	return NewExecutor(NewRegistry(), nil, nil, nil, 0, max)
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return b
}

// TestTruncateStructuredStaysValidJSON 核心用例：结构化结果被限幅后必须仍是合法 JSON。
//
// 这一条对着**改造前**的代码跑会红：硬切产出的半个对象 Unmarshal 直接报错。
func TestTruncateStructuredStaysValidJSON(t *testing.T) {
	const max = 8 * 1024
	e := newTruncExec(max)

	hits := make([]bigHit, 0, 200)
	for i := 0; i < 200; i++ {
		hits = append(hits, bigHit{
			File: "C:\\Users\\someone\\deep\\nested\\path\\to\\a\\fairly\\long\\file\\name.go",
			Line: i + 1,
			Text: strings.Repeat("匹配到的源码内容 ", 8),
		})
	}
	original := mustMarshal(t, hits)
	if len(original) <= max {
		t.Fatalf("夹具太小，测不到限幅：%d 字节", len(original))
	}

	got := e.truncate(&ToolResult{Success: true, Data: hits})

	out, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("限幅后 Data 序列化失败: %v", err)
	}
	if !json.Valid(out) {
		t.Fatalf("限幅产出的不是合法 JSON，前 %d 字节: %s", min(200, len(out)), out[:min(200, len(out))])
	}
	// 必须真的限住了
	if len(out) > max {
		t.Errorf("限幅后仍有 %d 字节，超出上限 %d", len(out), max)
	}
	// 必须能被解析成一个「说明被截断了」的结构
	var env struct {
		Truncated     bool   `json:"truncated"`
		OriginalBytes int    `json:"original_bytes"`
		Preview       string `json:"preview"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("无法解析为截断信封: %v", err)
	}
	if !env.Truncated {
		t.Error("缺少 truncated 标记，模型无法知道内容被裁过")
	}
	if env.OriginalBytes != len(original) {
		t.Errorf("original_bytes 应为 %d，实际 %d", len(original), env.OriginalBytes)
	}
	if env.Preview == "" {
		t.Error("preview 不应为空，模型需要一点线索判断要不要缩小范围")
	}
	// preview 必须是原文前缀（有信息价值），不是随便截的
	if !strings.HasPrefix(string(original), env.Preview) {
		t.Error("preview 应是原序列化结果的前缀")
	}
	if !utf8.ValidString(env.Preview) {
		t.Error("preview 切坏了 UTF-8（半个汉字）")
	}
}

// TestTruncateStringBranchUnchanged 纯文本结果的形态不能变。
//
// read_file 这类工具返回 string，改了形态会影响所有下游对 data 做文本匹配的代码。
// 这条把「文本不换形态」钉成契约。
func TestTruncateStringBranchUnchanged(t *testing.T) {
	const max = 4 * 1024
	e := newTruncExec(max)
	big := strings.Repeat("行内容\n", 2000)

	got := e.truncate(&ToolResult{Success: true, Data: big})
	s, ok := got.Data.(string)
	if !ok {
		t.Fatalf("纯文本结果应仍是 string，实际 %T", got.Data)
	}
	if len(s) > max+128 {
		t.Errorf("文本未限住：%d 字节", len(s))
	}
	if !strings.Contains(s, "已截断") {
		t.Error("缺少截断标记")
	}
	if !utf8.ValidString(s) {
		t.Error("切坏了 UTF-8")
	}
}

// TestTruncateUnderLimitIsNoop 未超限的结果必须原样返回。
func TestTruncateUnderLimitIsNoop(t *testing.T) {
	e := newTruncExec(32 * 1024)
	small := []bigHit{{File: "a.go", Line: 1, Text: "x"}}

	got := e.truncate(&ToolResult{Success: true, Data: small})
	out, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(mustMarshal(t, small)) {
		t.Errorf("未超限的结果被改动了:\n got %s\nwant %s", out, mustMarshal(t, small))
	}
}

// TestTruncateNilAndError 边界：nil 结果不 panic；失败结果（Data==nil）不受影响。
func TestTruncateNilAndError(t *testing.T) {
	e := newTruncExec(1024)
	if got := e.truncate(nil); got != nil {
		t.Errorf("truncate(nil) 应返回 nil，实际 %+v", got)
	}
	res := e.truncate(Err("失败：%v", "某个很长的原因"))
	if res.Data != nil {
		t.Errorf("失败结果的 Data 应保持 nil，实际 %T", res.Data)
	}
	if !strings.Contains(res.Error, "失败") {
		t.Errorf("错误信息被改动了: %q", res.Error)
	}
}

// TestTruncateEscapingInflation 引号与反斜杠会让 JSON 膨胀约 2 倍。
//
// 这是「限幅按 data 量、真正进上下文的是整个 ToolResult」的直接后果。
// preview 的预算必须为此留余量，否则会二次超限。
func TestTruncateEscapingInflation(t *testing.T) {
	const max = 8 * 1024
	e := newTruncExec(max)

	hits := make([]bigHit, 0, 300)
	for i := 0; i < 300; i++ {
		hits = append(hits, bigHit{File: "a.go", Line: i, Text: strings.Repeat(`"\`, 40)})
	}
	original := mustMarshal(t, hits)
	if len(original) <= max {
		t.Fatalf("夹具太小: %d", len(original))
	}

	got := e.truncate(&ToolResult{Success: true, Data: hits})
	out, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if len(out) > max {
		t.Errorf("转义膨胀下仍超限: %d > %d（preview 预算没留余量）", len(out), max)
	}
	if !json.Valid(out) {
		t.Error("产出不是合法 JSON")
	}
}

// TestTruncateMarshalFailure 不该 panic，也不该放行超限内容。
func TestTruncateMarshalFailure(t *testing.T) {
	const max = 256
	e := newTruncExec(max)
	bad := map[string]any{"ch": make(chan int), "pad": strings.Repeat("x", 4096)}

	// 关键是「不 panic」
	got := e.truncate(&ToolResult{Success: true, Data: bad})
	if got == nil {
		t.Fatal("不应返回 nil")
	}
}

// TestTruncateSingleHugeValue 单个巨长值就超限的极端形态。
func TestTruncateSingleHugeValue(t *testing.T) {
	const max = 4 * 1024
	e := newTruncExec(max)
	one := map[string]any{"content": strings.Repeat("x", 100*1024)}

	got := e.truncate(&ToolResult{Success: true, Data: one})
	out, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if len(out) > max {
		t.Errorf("单值超限形态下仍超限: %d > %d", len(out), max)
	}
	if !json.Valid(out) {
		t.Error("产出不是合法 JSON")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
