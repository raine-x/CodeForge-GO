package errs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
)

// ---------- 分类 ----------

func TestClassifySentinelErrors(t *testing.T) {
	cases := []struct {
		err  error
		want Kind
	}{
		{context.Canceled, KindCanceled},
		{context.DeadlineExceeded, KindTimeout},
		// 这条是本次改动的起因：SSE 长连接被掐断时底层给的就是它，
		// 之前被原样抛成 "unexpected EOF" 给用户看。
		{io.ErrUnexpectedEOF, KindStreamCut},
		{io.EOF, KindStreamCut},
		{fmt.Errorf("读流失败: %w", io.ErrUnexpectedEOF), KindStreamCut}, // 包装后仍能认出来
		{syscall.ECONNREFUSED, KindConnRefused},
		{syscall.ECONNRESET, KindConnReset},
		{syscall.ENOSPC, KindDiskFull},
		{os.ErrPermission, KindPermission},
		{os.ErrNotExist, KindNotFound},
	}
	for _, c := range cases {
		if got := Classify(c.err); got != c.want {
			t.Errorf("Classify(%v) = %v, 期望 %v", c.err, got, c.want)
		}
	}
}

func TestClassifyNetError(t *testing.T) {
	// DNS 失败必须归到「网络不通」，而不是靠字符串猜。
	dns := &net.DNSError{Err: "no such host", Name: "nope.invalid"}
	if got := Classify(dns); got != KindNetUnreachable {
		t.Errorf("DNS 错误应归为网络不通，实际 %v", got)
	}

	// 实现 net.Error 且 Timeout()==true 的，应归为超时。
	to := &timeoutErr{}
	if got := Classify(to); got != KindTimeout {
		t.Errorf("超时类 net.Error 应归为超时，实际 %v", got)
	}
}

type timeoutErr struct{}

func (e *timeoutErr) Error() string   { return "i/o timeout" }
func (e *timeoutErr) Timeout() bool   { return true }
func (e *timeoutErr) Temporary() bool { return true }

func TestClassifyParseError(t *testing.T) {
	var v map[string]any
	err := json.Unmarshal([]byte(`{"a":`), &v)
	if got := Classify(err); got != KindParse {
		t.Errorf("JSON 语法错误应归为格式错，实际 %v", got)
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := map[int]Kind{
		401: KindAuth,
		403: KindAuth, // 网关侧 403 常是密钥没同步，对用户都是「鉴权没过」
		429: KindRateLimit,
		500: KindUpstream,
		502: KindUpstream,
		503: KindUpstream,
		404: KindNotFound,
	}
	for code, want := range cases {
		if got := ClassifyStatus(code); got != want {
			t.Errorf("ClassifyStatus(%d) = %v, 期望 %v", code, got, want)
		}
	}
}

// 认不出来的错误必须落到 KindUnknown —— 宁可说「不知道」，也不要瞎归类。
func TestClassifyUnknown(t *testing.T) {
	if got := Classify(errors.New("某种完全没见过的故障")); got != KindUnknown {
		t.Errorf("未识别错误应为 KindUnknown，实际 %v", got)
	}
	if got := Classify(nil); got != KindUnknown {
		t.Errorf("nil 应为 KindUnknown，实际 %v", got)
	}
}

// ---------- 可重试性 ----------

func TestRetryable(t *testing.T) {
	retryable := []Kind{KindTimeout, KindNetUnreachable, KindConnReset, KindStreamCut, KindRateLimit, KindUpstream}
	for _, k := range retryable {
		if !Retryable(k) {
			t.Errorf("%v 应可重试", k)
		}
	}
	// 这几种重试一百次结果都一样，不该自动重试。
	notRetryable := []Kind{KindAuth, KindPermission, KindNotFound, KindParse, KindDiskFull, KindCanceled}
	for _, k := range notRetryable {
		if Retryable(k) {
			t.Errorf("%v 不应可重试", k)
		}
	}
	// 用户主动取消时自动重试是最招人烦的行为，必须明确排除。
	if Retryable(KindCanceled) {
		t.Error("KindCanceled 绝不能可重试")
	}
}

// ---------- 文案 ----------

func TestFriendly(t *testing.T) {
	// 已知类别：成因 + 建议
	msg := Friendly("保存模型库", syscall.ENOSPC)
	if !strings.HasPrefix(msg, "保存模型库失败：") {
		t.Errorf("应以「动作失败：」开头，实际 %q", msg)
	}
	if !strings.Contains(msg, "磁盘") {
		t.Errorf("应说明成因是磁盘，实际 %q", msg)
	}
	if !strings.Contains(msg, "清理") {
		t.Errorf("应给出处置建议，实际 %q", msg)
	}

	// 流被截断：必须说「传到一半断了」，不能是裸的 unexpected EOF
	msg = Friendly("生成回复", io.ErrUnexpectedEOF)
	if !strings.Contains(msg, "传到一半") {
		t.Errorf("流截断应说明是中途断开，实际 %q", msg)
	}
	if strings.Contains(msg, "unexpected EOF") {
		t.Errorf("不应把标准库原文直接给用户，实际 %q", msg)
	}

	// 未知类别：保留原始线索，不臆测
	msg = Friendly("打开文件", errors.New("某种完全没见过的故障"))
	if !strings.Contains(msg, "某种完全没见过的故障") {
		t.Errorf("未知错误应保留原始信息，实际 %q", msg)
	}

	// nil 错误：返回空串，不制造假错误
	if msg := Friendly("保存", nil); msg != "" {
		t.Errorf("nil 错误应返回空串，实际 %q", msg)
	}
}

// FriendlyOr：只补系统级故障，业务层写好的中文说明原样保留。
func TestFriendlyOr(t *testing.T) {
	// 业务错误：不该被包装
	biz := errors.New("子智能体越权：目标 /x 不在其允许范围 [/y] 内")
	if got := FriendlyOr("读取文件", biz); got != biz.Error() {
		t.Errorf("业务错误应原样返回，实际 %q", got)
	}

	// 系统错误：应补上成因与建议
	got := FriendlyOr("读取文件", io.ErrUnexpectedEOF)
	if !strings.HasPrefix(got, "读取文件失败：") {
		t.Errorf("系统错误应补充动作前缀，实际 %q", got)
	}
	if !strings.Contains(got, "传到一半") {
		t.Errorf("系统错误应说明成因，实际 %q", got)
	}

	// nil 不制造假错误
	if got := FriendlyOr("读取文件", nil); got != "" {
		t.Errorf("nil 应返回空串，实际 %q", got)
	}
}

// 上下文超窗：用 2026-09-21 实测的那条上游报错原文当用例。
//
// 这条错误看起来像「参数错」（invalid_request_error），若不单独识别，
// 会被当成普通 400 直接抛给用户 —— 表现为整轮任务白跑。
func TestClassifyContextOverflow(t *testing.T) {
	real := `LLM 请求失败 (400): {"error":{"code":"upstream_request_rejected",` +
		`"message":"This model's maximum context length is 262144 tokens. However, ` +
		`you requested 128000 output tokens and your prompt contains at least 134145 ` +
		`input tokens, for a total of at least 262145 tokens. Please reduce the length ` +
		`of the input prompt or the number of requested output tokens. ` +
		`(parameter=input_tokens, value=134145)","type":"invalid_request_error"}}`
	if got := Classify(errors.New(real)); got != KindContextOverflow {
		t.Fatalf("实测报错应归为上下文超窗，实际 %v", got)
	}

	// 各家文案
	others := []string{
		"prompt is too long: 210000 tokens > 200000 maximum",
		"context_length_exceeded",
		"The input token count exceeds the maximum number of tokens allowed",
		"this model's maximum context length is 8192 tokens",
	}
	for _, m := range others {
		if got := Classify(errors.New(m)); got != KindContextOverflow {
			t.Errorf("%q 应归为上下文超窗，实际 %v", m, got)
		}
	}
}

// 别把「限流 / 余额」误判成超窗 —— 它们的处理方式完全不同。
func TestContextOverflowDoesNotSwallowOtherErrors(t *testing.T) {
	notOverflow := []string{
		"rate limit exceeded, please retry later",
		"insufficient balance",
		"invalid api key",
		"request timeout",
	}
	for _, m := range notOverflow {
		if got := Classify(errors.New(m)); got == KindContextOverflow {
			t.Errorf("%q 不该被判为上下文超窗", m)
		}
	}
}

// 超窗必须**不可原样重试**：同一个请求再发一次还是超窗。
// 自救方式是由 agent 层「压缩后重发」，而不是让传输层盲目重试。
func TestContextOverflowIsNotBlindlyRetryable(t *testing.T) {
	if Retryable(KindContextOverflow) {
		t.Error("上下文超窗不该被当成可原样重试的瞬时故障")
	}
}

// 超窗的处置建议必须说清「系统会自己压」，否则用户会以为要手动清空历史。
func TestContextOverflowHintMentionsAutoCompress(t *testing.T) {
	h := Hint(KindContextOverflow)
	if !strings.Contains(h, "自动压缩") {
		t.Errorf("建议应说明会自动压缩，实际 %q", h)
	}
	if !strings.Contains(h, "输出上限") {
		t.Errorf("建议应给出可操作的调整项（输出上限），实际 %q", h)
	}
}

// Wrap 必须保留错误链，否则上层就没法再 errors.Is 判断了。
func TestWrapKeepsChain(t *testing.T) {
	base := fmt.Errorf("外层: %w", io.ErrUnexpectedEOF)
	wrapped := Wrap("拉取模型列表", base)
	if wrapped == nil {
		t.Fatal("不应返回 nil")
	}
	if !IsStreamCut(wrapped) {
		t.Error("Wrap 后应仍能识别出流被截断")
	}
}

func TestIsStreamCut(t *testing.T) {
	if !IsStreamCut(io.ErrUnexpectedEOF) {
		t.Error("io.ErrUnexpectedEOF 应判定为流截断")
	}
	if IsStreamCut(errors.New("正常错误")) {
		t.Error("普通错误不该判定为流截断")
	}
	if IsStreamCut(nil) {
		t.Error("nil 不该判定为流截断")
	}
}

// 每个 Kind 都要有中文成因；漏了会退化成空字符串，用户看到「失败：」就没了。
func TestEveryKindHasCause(t *testing.T) {
	all := []Kind{
		KindUnknown, KindTimeout, KindNetUnreachable, KindConnRefused, KindConnReset,
		KindStreamCut, KindTLS, KindAuth, KindRateLimit, KindUpstream,
		KindPermission, KindNotFound, KindDiskFull, KindParse, KindCanceled,
		KindContextOverflow, // 此前漏掉：有 Cause 实现却没被这条断言覆盖
		KindUnsupportedMedia,
	}
	for _, k := range all {
		if strings.TrimSpace(Cause(k)) == "" {
			t.Errorf("Kind %v 缺少中文成因说明", k)
		}
	}
}

// ---------------------------------------------------------------------------
// 拒收图片 / 视频
//
// 这一类此前会掉进 KindParse，界面上显示成「数据格式不对（解析失败）」——
// 用户看到「格式错误」只会去检查自己的图片有没有坏，真凶（模型看不见图）
// 永远不会被发现。这组测试钉住「它必须有自己的类别」。
// ---------------------------------------------------------------------------

func TestClassifyUnsupportedMedia(t *testing.T) {
	// 带状态码的实测形态：必须优先于 ClassifyStatus(400) = KindParse。
	real := NewStatus(400, `{"error":{"message":"Invalid content type in request: `+
		`'image_url' is not supported by this model","type":"invalid_request_error"}}`,
		0, nil)
	if got := Classify(real); got != KindUnsupportedMedia {
		t.Fatalf("上游拒收图片应归为 KindUnsupportedMedia，实际 %v", got)
	}

	// 各家文案（裸 error，不带状态码）
	others := []string{
		"The model does not support image inputs",
		"unsupported content type: image",
		"messages.1.content.0.type: unexpected content block type 'video_url'",
		"unexpected `image` content block",
		"该模型不支持图片输入",
		"模型不支持视频",
	}
	for _, m := range others {
		if got := Classify(errors.New(m)); got != KindUnsupportedMedia {
			t.Errorf("%q 应归为拒收媒体，实际 %v", m, got)
		}
	}
}

// 判据要求「模态名 + 拒绝词」同时出现，否则会误伤两类**处置方式完全不同**的错误：
// 图片太大（该换小图）、图片损坏（该换文件）。判错会让用户去改错的东西。
func TestUnsupportedMediaDoesNotSwallowSizeOrCorruption(t *testing.T) {
	notMedia := []string{
		"image exceeds the maximum allowed size",
		"image is too large, please use a smaller file",
		"failed to decode image: invalid JPEG data",
		"unsupported image format: bmp", // 格式级拒绝：换个文件就行，不该中止整轮
		"不支持该图片格式，请先转为 png",
		"this feature is not supported in your plan",
		"video_url is required for this endpoint",
		"unexpected EOF while reading image data", // 流截断，判据里的 "unexpected" 不该压过它
	}
	for _, m := range notMedia {
		if got := Classify(errors.New(m)); got == KindUnsupportedMedia {
			t.Errorf("%q 不该被判为「模型不支持媒体」", m)
		}
	}
}

// 不可原样重试：模型看不见图片，重发一万次上游还是拒。
func TestUnsupportedMediaIsNotBlindlyRetryable(t *testing.T) {
	if Retryable(KindUnsupportedMedia) {
		t.Error("拒收媒体不该被当成可原样重试的瞬时故障")
	}
}

// 成因与建议都必须说清「是模型的问题」并给出下一步，
// 否则用户只会反复检查自己的附件。
func TestUnsupportedMediaCauseAndHint(t *testing.T) {
	c := Cause(KindUnsupportedMedia)
	if !strings.Contains(c, "模型") {
		t.Errorf("成因应指向模型，实际 %q", c)
	}
	if strings.Contains(c, "格式") {
		t.Errorf("成因不该再出现「格式」（那正是要消掉的误导），实际 %q", c)
	}
	h := Hint(KindUnsupportedMedia)
	if !strings.Contains(h, "设置") {
		t.Errorf("建议应给出可操作入口（设置 → 模型），实际 %q", h)
	}
}
