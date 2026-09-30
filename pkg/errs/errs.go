// Package errs 把底层错误翻译成「给用户看的中文说明」。
//
// 为什么需要它
//
//	Go 标准库与系统调用的错误原文对用户毫无意义：
//	  "unexpected EOF"、"context deadline exceeded"、"Access is denied."、
//	  "dial tcp 1.2.3.4:443: connectex: A connection attempt failed..."
//	用户既看不出发生了什么，也不知道下一步该做什么。
//
//	本项目此前到处是 `"error": err.Error()` / `tools.Err("%v", err)`，
//	于是这些原文就直接落到了界面和模型上下文里。2026-09-21 实测：
//	一次 SSE 长连接被中途掐断，界面上只显示了一行 "unexpected EOF"。
//
// 设计原则
//
//  1. **不改写原始错误**：本包只负责「翻译成人话」，原始 error 仍可 errors.Is/As。
//  2. **成因 + 建议**：只报「失败了」没有价值，必须告诉用户是网络、是配置、
//     还是上游拒绝，以及能不能重试。
//  3. **可重试性显式化**：Retryable() 供上层决定要不要自动重试 / 提示用户重试。
//  4. **未知错误兜底**：认不出来时也要给出「动作 + 原始信息」，
//     既不丢线索，也不假装知道原因。
package errs

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"strings"
	"syscall"
)

// Kind 是底层错误的类别。分类的目的是决定「怎么跟用户说」和「能不能重试」。
type Kind int

const (
	// KindUnknown 认不出来的错误。兜底时保留原始信息，不臆测原因。
	KindUnknown Kind = iota
	// KindTimeout 超时：连上了但对方没在预期时间内回完。
	KindTimeout
	// KindNetUnreachable 网络不可达：DNS 解析不了，或根本连不上目标。
	KindNetUnreachable
	// KindConnRefused 目标拒绝连接：端口没开 / 服务没起。
	KindConnRefused
	// KindConnReset 连接被对端重置或中途掐断。
	KindConnReset
	// KindStreamCut 流式响应中途断开（响应体没读完就结束了）。
	KindStreamCut
	// KindTLS TLS/证书问题。
	KindTLS
	// KindAuth 鉴权失败：密钥无效、无权限、被上游拒绝。
	KindAuth
	// KindRateLimit 被限流 / 上游过载。
	KindRateLimit
	// KindUpstream 上游返回了 5xx。
	KindUpstream
	// KindPermission 本机文件/资源权限不足。
	KindPermission
	// KindNotFound 目标文件或资源不存在。
	KindNotFound
	// KindDiskFull 磁盘写满。
	KindDiskFull
	// KindParse 数据格式错误：JSON/YAML 解析失败。
	KindParse
	// KindContextOverflow 上游因「输入 + 输出超过模型上下文窗口」拒绝请求。
	//
	// 单独归类的原因：这类失败**不能直接报给用户就完事** —— 它是可自救的：
	// 把历史压得更狠一点、再发一次通常就过了。若当成普通 400 直接抛出去，
	// 用户看到的就是一整轮任务白跑（2026-09-21 实测：输入 134145 + 输出 128000
	// 超过 262144 窗口，上游回 upstream_request_rejected，整轮直接死掉）。
	KindContextOverflow
	// KindCanceled 被主动取消（用户点了停止、切走页面等）。
	KindCanceled
	// KindUnsupportedMedia 上游**拒收图片/视频**内容块：模型（或那条兼容网关）
	// 不具备该模态的输入能力。
	//
	// 单独归类的理由：它本来会掉进 KindParse（400 → 「数据格式不对，解析失败」），
	// 那条文案对用户毫无意义 —— 真实原因是「这个模型看不见图片」，而用户看到
	// 的是「格式错误」，于是会去检查自己的文件有没有坏，永远查不到真凶。
	// 更糟的是这类失败**不可重试且不可自救**：重发一百次仍是同一个结果。
	//
	// 上层靠它做两件事（见 agent 的能力门）：中止本轮并回滚已写入历史的
	// 媒体块（否则图片留在历史里，这个会话之后每次请求都被同样拒绝），
	// 以及记住该模型不支持，从此不再拿媒体去撞上游。
	KindUnsupportedMedia
)

// 常见 HTTP 状态码，供 ClassifyStatus 使用。
const (
	statusBadRequest    = 400
	statusUnauthorized  = 401
	statusForbidden     = 403
	statusNotFound      = 404
	statusTooManyReq    = 429
	statusServerErrFrom = 500
)

// Classify 判断错误属于哪一类。
//
// 判定顺序有讲究：先看哨兵错误（context / io），再看接口型错误（net.Error），
// 最后才做字符串兜底 —— 字符串匹配最不可靠，放最后。
func Classify(err error) Kind {
	if err == nil {
		return KindUnknown
	}

	// ---- 1. 哨兵错误：最可靠 ----
	switch {
	case errors.Is(err, context.Canceled):
		return KindCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return KindTimeout
	case errors.Is(err, io.ErrUnexpectedEOF):
		// 响应体长度对不上 = 流被中途掐断。这是最容易被误报成
		// 「文件读错」的一类，必须单独归类。
		return KindStreamCut
	case errors.Is(err, io.EOF):
		// 裸 io.EOF 出现在「读流」语境下，通常也是对方提前关了连接。
		return KindStreamCut
	}

	// ---- 1.5 HTTP 状态码：最可靠，且能覆盖 body 里没有任何关键词的上游 ----
	//
	// 必须排在字符串兜底之前。实测 GLM 的 429 body 是
	// {"code":"1305","message":"你设置的当前模型正在被其他人使用，请稍后重试"}，
	// 一个 "rate limit" / "too many requests" 都没有；401 同理。
	// 只靠猜文本这两类一律掉进 KindUnknown —— 限流不触发退避建议、
	// 鉴权失败不触发「去检查密钥」的建议，用户在界面上看到的是裸 JSON。
	if code := StatusCode(err); code > 0 {
		// ⚠️ 400 里混着「上下文超窗」，它不是格式错而是**可自救**的拒绝：
		// agent 靠 KindContextOverflow 驱动「压缩历史后重发」这条命脉。
		// 所以超窗必须先判，否则这里会把 400 直接判成 KindParse，自救路断掉，
		// 而且既有的 TestClassifyContextOverflow（用裸 errors.New）抓不到。
		if isContextOverflowMessage(strings.ToLower(err.Error())) {
			return KindContextOverflow
		}
		// ⚠️ 「拒收图片/视频」同样必须先于 ClassifyStatus：它也常是 400，
		// 而 ClassifyStatus(400) = KindParse →「数据格式不对（解析失败）」。
		// 不在这里截住，用户看到的就是「格式错误」，永远想不到是模型看不见图，
		// 于是去反复检查自己的图片有没有坏 —— 真凶在模型侧，跟文件无关。
		if isUnsupportedMediaMessage(strings.ToLower(err.Error())) {
			return KindUnsupportedMedia
		}
		if k := ClassifyStatus(code); k != KindUnknown {
			return k
		}
		// 有状态码但不落在已知区间：按上游故障处理。
		// 比 KindUnknown 更好 —— 上游确实明确报错了，提示语应说清是上游问题。
		return KindUpstream
	}

	// ---- 2. 网络错误：用接口判断，不猜字符串 ----
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return KindNetUnreachable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return KindTimeout
		}
		// 非超时的 net.Error 继续往下走，靠 syscall 细分
	}

	// ---- 3. 系统调用错误 ----
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return KindConnRefused
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return KindConnReset
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return KindNetUnreachable
	case errors.Is(err, syscall.ENOSPC):
		return KindDiskFull
	case errors.Is(err, fs.ErrPermission), errors.Is(err, syscall.EACCES):
		return KindPermission
	case errors.Is(err, fs.ErrNotExist):
		return KindNotFound
	}

	// ---- 4. 证书 ----
	var certErr *x509.CertificateInvalidError
	var hostErr x509.HostnameError
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &certErr) || errors.As(err, &hostErr) || errors.As(err, &unknownAuth) {
		return KindTLS
	}

	// ---- 5. 数据格式 ----
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return KindParse
	}

	// ---- 6. 字符串兜底：认不出来时才用，且只做保守匹配 ----
	msg := strings.ToLower(err.Error())
	// 上下文超窗要**先于**其它判断：各家文案差异很大，且这类错误常带
	// "invalid_request_error" 这种看起来像参数错的字样，容易被误归类。
	if isContextOverflowMessage(msg) {
		return KindContextOverflow
	}
	switch {
	case strings.Contains(msg, "unexpected eof"),
		strings.Contains(msg, "stream closed"),
		strings.Contains(msg, "response body closed"),
		strings.Contains(msg, "http2: client connection lost"):
		return KindStreamCut
	case strings.Contains(msg, "context deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "timed out"):
		return KindTimeout
	case strings.Contains(msg, "connection refused"):
		return KindConnRefused
	case strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "forcibly closed"):
		return KindConnReset
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "network is unreachable"),
		strings.Contains(msg, "cannot find the host"),
		strings.Contains(msg, "dns"):
		return KindNetUnreachable
	case strings.Contains(msg, "x509"), strings.Contains(msg, "certificate"):
		return KindTLS
	case strings.Contains(msg, "rate limit"), strings.Contains(msg, "too many requests"):
		return KindRateLimit
	case strings.Contains(msg, "unauthorized"), strings.Contains(msg, "invalid api key"),
		strings.Contains(msg, "authentication"):
		return KindAuth
	case strings.Contains(msg, "access is denied"), strings.Contains(msg, "permission denied"):
		return KindPermission
	case strings.Contains(msg, "no space left"):
		return KindDiskFull
	case strings.Contains(msg, "unexpected end of json"), strings.Contains(msg, "invalid character"):
		return KindParse
	}
	// 拒收媒体放在**最后**：它的判据含 "unexpected" 这个宽词，
	// 必须让前面那些更精确的判断先跑完 —— 否则「unexpected EOF of image」
	// 会被当成「模型不支持图片」，把一次流截断误判成能力问题并中止整轮。
	if isUnsupportedMediaMessage(msg) {
		return KindUnsupportedMedia
	}
	return KindUnknown
}

// isContextOverflowMessage 识别「输入 + 输出超过上下文窗口」这类上游拒绝。
//
// msg 必须是**小写**后的错误全文。
//
// 各家文案差异很大，这里覆盖已实测/已知的几种：
//
//	OpenAI / 多数兼容网关:
//	  "This model's maximum context length is 262144 tokens. However, you requested
//	   128000 output tokens and your prompt contains at least 134145 input tokens…"
//	Anthropic:
//	  "prompt is too long: 210000 tokens > 200000 maximum"
//	Google:
//	  "The input token count exceeds the maximum number of tokens allowed"
//	通用:
//	  "context_length_exceeded" / "reduce the length of the input"
//
// 判据刻意用「窗口/长度」这类词，而不是单独匹配 "tokens" ——
// 否则会把限流、余额不足等也误判成超窗。
func isContextOverflowMessage(msg string) bool {
	patterns := []string{
		"maximum context length",
		"context length exceeded",
		"context_length_exceeded",
		"exceeds the maximum number of tokens",
		"reduce the length of the input",
		"prompt is too long",
		"input is too long",
		"too many tokens",
		"maximum number of tokens",
		"exceeds the context window",
		"context window exceeded",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isUnsupportedMediaMessage 识别「上游拒收图片/视频内容块」。
//
// msg 必须是**小写**后的错误全文。
//
// 判据刻意要求**同时**出现「模态名」与「拒绝/不支持」两类词：
// 只匹配 "image" 会把「图片过大（too large）」「图片已损坏」这类同样提到
// image 的错误误判成「模型不支持图片」—— 而那两类的正确处置完全不同
// （换小图 / 换文件），判错会让用户去改错的东西。只匹配 "unsupported" 会把
// 「该功能在你的订阅计划中不支持」这类无关的 403 也拖进来。
//
// 已实测/已知的文案：
//
//	OpenAI 兼容层:
//	  "Invalid content type in request... 'image_url' is not supported"
//	  "The model does not support image inputs" / "unsupported content type: image"
//	Anthropic:
//	  "messages.1.content.0.type: unexpected content block type 'video_url'"
//	  "unexpected `image` content block"
//	中文网关（GLM / 通义 / DeepSeek 一类）:
//	  "该模型不支持图片输入" / "模型不支持视频"
//
// **排除格式级拒绝**："unsupported image format: bmp" 说的是这**一个文件**
// 的编码不被接受，而 "model does not support image" 说的是这个**模型**
// 根本收不到图片。前者换个文件就好（该报错、该让用户去转格式），
// 后者只能换模型（该中止、该记住）——判错会让用户去改一个根本不用改的东西。
func isUnsupportedMediaMessage(msg string) bool {
	if !hasAnyToken(msg, "image", "video", "图片", "图像", "视频") {
		return false
	}
	if hasAnyToken(msg, "image format", "video format", "图片格式", "视频格式", "格式不支持") {
		return false
	}
	return hasAnyToken(msg,
		"not support", "unsupported", "unexpected",
		"invalid content type", "unknown content type",
		"不支持", "无法处理", "不能识别",
	)
}

// hasAnyToken 报告 msg 是否含有其中任一子串。**不做分词切分**：
// 这些词都是 ASCII 词组或完整中文词，出现在错误正文里几乎不会有歧义，
// 而引入分词依赖会让 errs 依赖标准库之外的东西 —— 这个包的定位就是零依赖兜底。
func hasAnyToken(msg string, tokens ...string) bool {
	for _, t := range tokens {
		if strings.Contains(msg, t) {
			return true
		}
	}
	return false
}

// ClassifyStatus 按 HTTP 状态码归类。上游返回非 2xx 时用它。
func ClassifyStatus(code int) Kind {
	switch {
	case code == statusUnauthorized || code == statusForbidden:
		// 403 在网关侧常是「密钥没同步 / 被风控」，对用户来说都是「鉴权没过」。
		return KindAuth
	case code == statusTooManyReq:
		return KindRateLimit
	case code >= statusServerErrFrom:
		return KindUpstream
	case code == statusNotFound:
		return KindNotFound
	case code == statusBadRequest:
		return KindParse
	}
	return KindUnknown
}

// Cause 给出该类错误的**中文成因**。用于「<动作>失败：<成因>」的前半段。
func Cause(k Kind) string {
	switch k {
	case KindTimeout:
		return "请求超时（对方在预期时间内没有返回）"
	case KindNetUnreachable:
		return "网络不通（域名解析不了，或连不上目标地址）"
	case KindConnRefused:
		return "目标拒绝连接（服务没启动，或端口不对）"
	case KindConnReset:
		return "连接被中断（对端或中间的网络设备把连接掐断了）"
	case KindStreamCut:
		return "响应传到一半连接就断了（流式响应被中途截断）"
	case KindTLS:
		return "HTTPS 证书校验失败"
	case KindAuth:
		return "鉴权失败（密钥无效、无权限，或被上游拒绝）"
	case KindRateLimit:
		return "被上游限流（请求太频繁或额度用尽）"
	case KindUpstream:
		return "上游服务出错"
	case KindPermission:
		return "权限不足（没有读写该文件/目录的权限）"
	case KindNotFound:
		return "找不到目标（文件或资源不存在）"
	case KindDiskFull:
		return "磁盘空间不足"
	case KindParse:
		return "数据格式不对（解析失败）"
	case KindContextOverflow:
		return "输入 + 输出超过了模型的上下文窗口"
	case KindCanceled:
		return "操作被取消"
	case KindUnsupportedMedia:
		// 不猜是图片还是视频：同一条判据同时覆盖两种模态，
		// 猜错会让用户去检查他根本没附的那类文件。
		// 「附件」是两者共同的上位词 —— 界面上删掉的那一栏就是这个。
		return "当前模型不支持接收图片/视频附件"
	}
	return "发生了未预期的错误"
}

// Hint 给出该类错误的**处置建议**。返回空串表示没有额外建议。
func Hint(k Kind) string {
	switch k {
	case KindTimeout:
		return "可以稍后重试；若持续超时，检查网络或代理设置"
	case KindNetUnreachable:
		return "检查网络连接、代理设置，或确认该地址在当前网络下可达"
	case KindConnRefused:
		return "确认目标服务已启动、地址与端口填写正确"
	case KindConnReset:
		return "通常是网络抖动，重试即可；频繁出现请检查代理/VPN 稳定性"
	case KindStreamCut:
		return "多为网络或代理抖动，重试一般即可恢复"
	case KindTLS:
		return "检查系统时间是否正确，或该地址是否被中间设备拦截"
	case KindAuth:
		return "到「设置 → 模型」检查该供应商的密钥是否有效、是否已过期"
	case KindRateLimit:
		return "降低请求频率，或稍后再试"
	case KindUpstream:
		return "这是上游服务的问题，稍后重试；持续失败请换个模型或供应商"
	case KindPermission:
		return "检查该文件/目录的权限，或换一个可写的位置"
	case KindNotFound:
		return "确认路径是否正确"
	case KindDiskFull:
		return "清理磁盘空间后重试"
	case KindParse:
		return "确认数据来源完整、格式正确"
	case KindContextOverflow:
		// 这条建议很重要：它不是「你哪里填错了」，而是「对话太长了」，
		// 且系统**会自己压缩后重试** —— 要让用户知道不用手动清空历史。
		return "系统会自动压缩历史后重试；若反复出现，请在「设置 → 模型」调小该条目的「输出上限」"
	case KindCanceled:
		return ""
	case KindUnsupportedMedia:
		// 这条建议要给出**下一步能做什么**，否则用户只能干瞪眼：
		// 换模型（模型库里勾上能力声明）、或把内容转成文字描述再问一次。
		return "在「设置 → 模型」里给该模型勾上「支持图片 / 支持视频」声明（仅当它确实支持时），或改用支持视觉的模型；也可以直接把内容用文字描述给你看"
	}
	return ""
}

// Retryable 报告该类错误是否值得**原样重试**。
//
// 用途：上层据此决定「自动重试」还是「直接报错让用户处理」。
//
// ⚠️ 注意 KindContextOverflow 返回 false —— 它需要的是**压缩后重发**，
// 而不是把同一个超窗请求再发一遍（那只会再被拒一次）。
// 调用方要单独识别它并走「先压缩、再重试」的路径（见 pkg/agent 的步骤循环）。
//
// KindUnsupportedMedia 同理不可重试：模型看不见图片这件事，重发一万次
// 上游还是会拒。它需要的是**换模型**或**去掉附件**，两者都在请求之外。
func Retryable(k Kind) bool {
	switch k {
	case KindTimeout, KindNetUnreachable, KindConnReset, KindStreamCut,
		KindRateLimit, KindUpstream, KindTLS:
		return true
	}
	// 鉴权失败、权限不足、格式错误、文件不存在 —— 重试一百次也是同样的结果。
	// 上下文超窗同理：不改变请求内容，重发没有意义。
	return false
}

// Friendly 组装一句完整的、给用户看的错误说明。
//
//	action 是「正在做什么」，用动宾短语，如「保存模型库」「拉取模型列表」。
//
// 输出形如：
//
//	保存模型库失败：磁盘空间不足。清理磁盘空间后重试
//	拉取模型列表失败：网络不通（域名解析不了，或连不上目标地址）。检查网络连接、代理设置…
//
// 认不出类别时退化为「<动作>失败：<原始信息>」—— 保留线索，不臆测原因。
func Friendly(action string, err error) string {
	if err == nil {
		return ""
	}
	k := Classify(err)
	if k == KindUnknown {
		return fmt.Sprintf("%s失败：%s", action, err.Error())
	}
	msg := fmt.Sprintf("%s失败：%s", action, Cause(k))
	if h := Hint(k); h != "" {
		msg += "。" + h
	}
	return msg
}

// Friendlyf 与 Friendly 相同，但 action 支持格式化。
func Friendlyf(err error, format string, a ...any) string {
	return Friendly(fmt.Sprintf(format, a...), err)
}

// FriendlyOr 只在能识别出**系统级故障**时才补充成因与建议；
// 已经是人话的业务错误原样返回，不做无谓包装。
//
// 为什么需要这个变体：项目里大量错误本身就是给用户看的中文说明，例如
// 「子智能体越权：目标 X 不在其允许范围 Y 内」「该操作需要人工审批」。
// 若一律套上 Friendly，会变成「读取文件失败：子智能体越权：目标 X 不在…」——
// 多一层废话，还容易让人误以为是文件读取本身出了问题。
//
// 判定依据：Classify 认不出来（KindUnknown）= 不是标准库/系统调用抛的，
// 多半是业务层精心写好的说明，此时**保持原样**最稳妥。
func FriendlyOr(action string, err error) string {
	if err == nil {
		return ""
	}
	if Classify(err) == KindUnknown {
		return err.Error()
	}
	return Friendly(action, err)
}

// wrapped 同时携带「给人看的说明」与「原始错误」。
//
// 为什么不用 fmt.Errorf("%s: %w", ...)：那会把标准库原文也拼进 Error()，
// 用户又会看到 "unexpected EOF"。这里让 Error() 只返回可读说明，
// 原始错误藏在 Unwrap() 里 —— 既人话，又保住 errors.Is/As。
type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }

// Wrap 把底层错误包成「可读说明 + 原始错误」。
//
// 与 Friendly 的区别：Wrap 返回 error，且**保留错误链**（Unwrap），
// 供需要继续 errors.Is/As 的调用方使用；Friendly 只返回给人看的字符串。
func Wrap(action string, err error) error {
	if err == nil {
		return nil
	}
	return &wrapped{msg: Friendly(action, err), err: err}
}

// IsStreamCut 报告错误是否为「流式响应被中途截断」。
//
// 单独暴露是因为 LLM 层要据此决定重试策略：**本轮若尚未产出任何内容，
// 重试是完全安全的**（不会产生重复输出）；已产出内容则不能静默重试。
func IsStreamCut(err error) bool {
	return Classify(err) == KindStreamCut
}
