package agent

import (
	"context"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/errs"
	"codeforge/pkg/llm"
)

// 能力门测试的前提：记忆是**进程级**的（见 capability.go 的 mediaMemory），
// 所以每个用例都用独立模型名，避免相互污染。这是刻意的：宁可名字啰嗦，
// 也不能让一个用例的失败连带搞红另一个。
const (
	modelPlain       = "cap-test-plain"
	modelNoVision    = "cap-test-no-vision"
	modelNoVideo     = "cap-test-no-video"
	modelRejectsImg  = "cap-test-rejects-image"
	modelRejectsBoth = "cap-test-rejects-both"
	modelVideoOnly   = "cap-test-video-only"
)

func boolPtr(v bool) *bool { return &v }

// resetMemory 清空能力记忆，让每个用例从干净状态开始。
func resetMemory(t *testing.T) {
	t.Helper()
	rejectedMedia.mu.Lock()
	rejectedMedia.rejected = nil
	rejectedMedia.mu.Unlock()
	t.Cleanup(func() {
		rejectedMedia.mu.Lock()
		rejectedMedia.rejected = nil
		rejectedMedia.mu.Unlock()
	})
}

var (
	imageBlock = llm.ContentBlock{Type: llm.BlockImage, MediaType: "image/png", Data: "aW1hZ2U="}
	videoBlock = llm.ContentBlock{Type: llm.BlockVideo, MediaType: "video/mp4", Data: "dmlkZW8="}
)

// newMediaSession 建一个带会话的测试 Agent（能力门用例都要检查历史）。
func newMediaSession(t *testing.T, p llm.Provider, cfg config.LLMConfig) (*Agent, *Session) {
	t.Helper()
	a := newEmitTestAgentWithCfg(t, p, cfg)
	sess, err := a.history.Create("", "capability")
	if err != nil {
		t.Fatal(err)
	}
	return a, sess
}

// countMediaBlocks 数历史里还剩几个媒体块。
func countMediaBlocks(sess *Session) int {
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	n := 0
	for _, m := range sess.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage || b.Type == llm.BlockVideo {
				n++
			}
		}
	}
	return n
}

// countMediaInRequest 数一次请求里有几个媒体块。
func countMediaInRequest(req llm.Request) int {
	n := 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage || b.Type == llm.BlockVideo {
				n++
			}
		}
	}
	return n
}

// sessHasText 报告历史里是否还留着某段文字。
func sessHasText(sess *Session, want string) bool {
	sess.mu.RLock()
	defer sess.mu.RUnlock()
	for _, m := range sess.Messages {
		for _, b := range m.Content {
			if strings.Contains(b.Text, want) {
				return true
			}
		}
	}
	return false
}

// hasInfoMentioning 报告是否有一条提示事件提到了某个关键词。
func hasInfoMentioning(events []Event, keyword string) bool {
	for _, ev := range events {
		if ev.Type == EventInfo && strings.Contains(ev.Text, keyword) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 判定：mediaVerdict
// ---------------------------------------------------------------------------

// 未声明必须等于「不知道」，绝不能等于「不支持」。
//
// 这条是整个设计的立足点：models.yaml 由设置页生成且被 gitignore，
// 绝大多数条目根本没有 vision/video 字段。若把「没写」读成 false，
// 每一个未声明的模型都从此收不到图片 —— 而用户没有任何入口去纠正它。
func TestUndeclaredMeansUnknownNotUnsupported(t *testing.T) {
	resetMemory(t)
	cfg := config.LLMConfig{Model: modelPlain} // Vision / Video 都是 nil
	for _, b := range []llm.ContentBlock{imageBlock, videoBlock} {
		if got, _ := mediaVerdict(cfg, b); got != mediaSend {
			t.Errorf("未声明能力时 %s 应乐观送出（保持既有行为），实际 %v", b.Type, got)
		}
	}
}

// 显式声明不支持 → 立刻拦下，一次请求都不该发出去。
func TestDeclaredUnsupportedIsBlockedBeforeSending(t *testing.T) {
	resetMemory(t)
	cfg := config.LLMConfig{Model: modelNoVision, Vision: boolPtr(false)}
	if got, why := mediaVerdict(cfg, imageBlock); got != mediaDropImage {
		t.Errorf("声明 vision:false 时图片应被拦下，实际 %v（%s）", got, why)
	}
	// 但视频没声明，仍要乐观送 —— 不能因为「图片不行」就连坐视频。
	if got, _ := mediaVerdict(cfg, videoBlock); got != mediaSend {
		t.Errorf("只声明了 vision:false 不该影响视频，实际 %v", got)
	}
}

// 两个字段是**独立**的：能收图不等于能收视频，反之亦然。
func TestVisionAndVideoAreIndependent(t *testing.T) {
	resetMemory(t)
	visionOnly := config.LLMConfig{Model: modelVideoOnly, Vision: boolPtr(true), Video: boolPtr(false)}
	if got, _ := mediaVerdict(visionOnly, imageBlock); got != mediaSend {
		t.Errorf("vision:true 时图片应可送，实际 %v", got)
	}
	if got, why := mediaVerdict(visionOnly, videoBlock); got != mediaDropVideo {
		t.Errorf("video:false 时视频应被跳过，实际 %v（%s）", got, why)
	}
}

// 视频走「跳过」，图片走「中止」—— 两者的区别是刻意的：
// 图片往往是提问的本体（报错截图），丢掉它继续答等于给一个自信的错误答案；
// 视频通常只是补充材料，为它中止整轮不划算。
func TestImageAbortsButVideoOnlySkips(t *testing.T) {
	resetMemory(t)
	cfg := config.LLMConfig{Model: modelNoVision, Vision: boolPtr(false), Video: boolPtr(false)}
	if got, _ := mediaVerdict(cfg, imageBlock); got != mediaDropImage {
		t.Errorf("图片应为「中止」，实际 %v", got)
	}
	if got, _ := mediaVerdict(cfg, videoBlock); got != mediaDropVideo {
		t.Errorf("视频应为「跳过」，实际 %v", got)
	}
}

// 显式声明「支持」必须**压过**记忆。
//
// 否则用户在设置页勾上「支持图片」并保存，界面显示成功、配置也落盘了，
// 行为却不变 —— 表现为「勾了也没用，重启一下就好了」。
func TestDeclaredSupportedOverridesMemory(t *testing.T) {
	resetMemory(t)
	rejectedMedia.note(modelRejectsImg, "image")
	if !rejectedMedia.remembers(modelRejectsImg, "image") {
		t.Fatal("记忆没写进去")
	}
	// 未声明 + 已记住 → 拦下（少撞一次墙）
	if got, _ := mediaVerdict(config.LLMConfig{Model: modelRejectsImg}, imageBlock); got != mediaDropImage {
		t.Errorf("未声明且已记住时图片应被拦下，实际 %v", got)
	}
	// 声明支持 → 放行
	declared := config.LLMConfig{Model: modelRejectsImg, Vision: boolPtr(true)}
	if got, _ := mediaVerdict(declared, imageBlock); got != mediaSend {
		t.Error("显式声明 vision:true 应压过此前的失败记忆")
	}
	// ForgetModel 后记忆清空
	if !ForgetMediaCapability(modelRejectsImg) {
		t.Error("ForgetMediaCapability 应报告清掉了东西")
	}
	if rejectedMedia.remembers(modelRejectsImg, "image") {
		t.Error("ForgetModel 之后不该还记得")
	}
}

// 记忆按「模型 × 模态」记：一次视频失败不该顺手把图片能力也关掉
// （Qwen-VL 一类正是「收图片、不收视频」）。
func TestMemoryIsPerModality(t *testing.T) {
	resetMemory(t)
	rejectedMedia.note(modelRejectsBoth, "video")
	if !rejectedMedia.remembers(modelRejectsBoth, "video") {
		t.Error("视频应已被记住")
	}
	if rejectedMedia.remembers(modelRejectsBoth, "image") {
		t.Error("视频被拒不该连带记住图片")
	}
}

// 不同模型之间不能串味：模型 A 拒收不影响模型 B。
func TestMemoryIsPerModel(t *testing.T) {
	resetMemory(t)
	rejectedMedia.note(modelRejectsImg, "image")
	if rejectedMedia.remembers(modelPlain, "image") {
		t.Error("记忆不该跨模型串味")
	}
}

// 模态是**排除法**定的，不猜。
// 图片与视频同轮送出而上游拒收时，先记视频（它本就是可跳过的那一类）；
// 若真正被拒的是图片，下一轮视频已不在场、再撞一次就会把图片也记上。
// 代价是多一次失败请求，换来的是不会猜错。
func TestNoteRejectedMediaPrefersVideoWhenBothSent(t *testing.T) {
	resetMemory(t)
	cfg := config.LLMConfig{Model: modelRejectsBoth}
	noteRejectedMedia(cfg, []llm.ContentBlock{imageBlock, videoBlock})
	if !rejectedMedia.remembers(modelRejectsBoth, "video") {
		t.Error("两者同送时应先记视频")
	}
	if rejectedMedia.remembers(modelRejectsBoth, "image") {
		t.Error("两者同送时不该同时记图片（那是猜）")
	}

	// 只有图片 → 结论唯一
	noteRejectedMedia(cfg, []llm.ContentBlock{imageBlock})
	if !rejectedMedia.remembers(modelRejectsBoth, "image") {
		t.Error("只送图片时应记图片")
	}

	// 什么都没送却报拒收（如协议层拒绝历史残留）→ 不记。
	// 硬记会把一个本来没问题的模型误标成不支持，代价远大于多撞一次墙。
	noteRejectedMedia(cfg, nil)
	if rejectedMedia.remembers(modelRejectsBoth, "text") {
		t.Error("没有媒体块时不该记入任何模态")
	}
}

// ---------------------------------------------------------------------------
// 摘除：stripMediaFromMessage
// ---------------------------------------------------------------------------

// 媒体块留在历史里，这个会话之后每轮都会被重发、每轮都被拒 —— 会话报废。
// 摘掉必须只动媒体块：用户的问题本身没问题，不该一起丢。
func TestStripMediaKeepsText(t *testing.T) {
	msg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		{Type: llm.BlockText, Text: "这个报错怎么修"},
		imageBlock, videoBlock,
		{Type: llm.BlockText, Text: "顺便看下这段"},
	}}
	if removed := stripMediaFromMessage(&msg); removed != 2 {
		t.Fatalf("应摘掉 2 个媒体块，实际 %d", removed)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("应只剩 2 个文本块，实际 %d", len(msg.Content))
	}
	for _, b := range msg.Content {
		if b.Type != llm.BlockText {
			t.Errorf("残留了非文本块: %+v", b)
		}
	}
	if !strings.Contains(msg.Content[0].Text, "这个报错怎么修") {
		t.Error("用户的问题原文必须保留")
	}
}

// 没有媒体块时不能动内容（否则会把一条纯文本消息改成空消息）。
func TestStripMediaNoopWhenNoMedia(t *testing.T) {
	msg := llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "hi"}}}
	if removed := stripMediaFromMessage(&msg); removed != 0 {
		t.Errorf("无媒体时不该报告摘除，实际 %d", removed)
	}
	if len(msg.Content) != 1 {
		t.Error("无媒体时不该动内容")
	}
}

// ---------------------------------------------------------------------------
// 端到端：RunWithMedia
// ---------------------------------------------------------------------------

// 声明 vision:false 时，图片不该走到上游，且整轮要中止 ——
// 因为丢掉图继续答 = 给一个自信的错误答案。
func TestRunWithMediaAbortsOnDeclaredNoVision(t *testing.T) {
	resetMemory(t)
	p := &imageRequestProvider{}
	a, sess := newMediaSession(t, p, config.LLMConfig{Model: modelNoVision, Vision: boolPtr(false)})

	var events []Event
	err := a.RunWithMedia(context.Background(), sess.ID, "这个报错怎么修", []llm.ContentBlock{imageBlock},
		func(ev Event) { events = append(events, ev) })
	if err == nil {
		t.Fatal("图片送不出去时必须中止，不能继续答")
	}
	if got := errs.Classify(err); got != errs.KindUnsupportedMedia {
		t.Errorf("错误应归为拒收媒体，实际 %v", got)
	}
	if len(p.requests) != 0 {
		t.Error("声明不支持时不该把图片发给上游")
	}
	if !hasInfoMentioning(events, "不支持") {
		t.Errorf("应明确告诉用户原因，实际事件: %+v", events)
	}
	// 历史里不该留下媒体块，也不该留下这一轮（中止在入历史之前）。
	if n := countMediaBlocks(sess); n != 0 {
		t.Errorf("中止后不该有媒体块留在历史，实际 %d", n)
	}
}

// 视频被跳过时**不中止**：问题还在，答案不因少一段视频而失效。
func TestRunWithMediaSkipsVideoAndContinues(t *testing.T) {
	resetMemory(t)
	p := &imageRequestProvider{}
	a, sess := newMediaSession(t, p, config.LLMConfig{Model: modelNoVideo, Video: boolPtr(false)})

	var events []Event
	if err := a.RunWithMedia(context.Background(), sess.ID, "这段视频讲了什么", []llm.ContentBlock{videoBlock},
		func(ev Event) { events = append(events, ev) }); err != nil {
		t.Fatalf("视频被跳过不该让整轮失败: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("请求应正常发出，实际 %d 次", len(p.requests))
	}
	if n := countMediaInRequest(p.requests[0]); n != 0 {
		t.Errorf("被跳过的视频不该进请求，实际 %d 个媒体块", n)
	}
	if !hasInfoMentioning(events, "视频") {
		t.Errorf("应提示视频被跳过（而不是静默丢掉），实际事件: %+v", events)
	}
	if n := countMediaBlocks(sess); n != 0 {
		t.Errorf("被跳过的视频不该进历史，实际 %d", n)
	}
}

// 未声明 + 上游拒收 → 显式中止、记住该模型、**并把媒体块从历史摘掉**。
// 三者缺一，这个会话就会在下一轮再次被同样拒绝。
func TestRunWithMediaRemembersAndRollsBackOnUpstreamRejection(t *testing.T) {
	resetMemory(t)
	p := &imageRequestProvider{
		streamErr: errs.NewUnsupportedMedia("'image_url' is not supported by this model"),
	}
	a, sess := newMediaSession(t, p, config.LLMConfig{Model: modelRejectsImg})

	err := a.RunWithMedia(context.Background(), sess.ID, "这个报错怎么修", []llm.ContentBlock{imageBlock},
		func(Event) {})
	if err == nil {
		t.Fatal("上游拒收时必须报错，不能当作成功")
	}
	if got := errs.Classify(err); got != errs.KindUnsupportedMedia {
		t.Errorf("应原样透出拒收媒体类别，实际 %v", got)
	}
	if !rejectedMedia.remembers(modelRejectsImg, "image") {
		t.Error("应记住该模型不收图片")
	}
	if n := countMediaBlocks(sess); n != 0 {
		t.Errorf("历史里的媒体块必须被摘掉（否则此后每轮都被拒），实际还剩 %d", n)
	}
	// 问题本身要还在 —— 用户不该因为附件发不出去就丢掉提问。
	if !sessHasText(sess, "这个报错怎么修") {
		t.Error("用户的提问文本必须保留在历史里")
	}
}

// 记住之后，第二轮就该**前置拦下**，一次请求都不发。
func TestSecondTurnBlockedWithoutHittingUpstream(t *testing.T) {
	resetMemory(t)
	p := &imageRequestProvider{
		streamErr: errs.NewUnsupportedMedia("'image_url' is not supported by this model"),
	}
	a, sess := newMediaSession(t, p, config.LLMConfig{Model: modelRejectsImg})

	if err := a.RunWithMedia(context.Background(), sess.ID, "第一轮", []llm.ContentBlock{imageBlock}, func(Event) {}); err == nil {
		t.Fatal("第一轮应失败")
	}
	calls := len(p.requests)

	// 第二轮：同样的附件。能力门此时已知道该模型不支持。
	err := a.RunWithMedia(context.Background(), sess.ID, "第二轮", []llm.ContentBlock{imageBlock}, func(Event) {})
	if err == nil {
		t.Fatal("第二轮仍应中止（图片送不出去）")
	}
	if len(p.requests) != calls {
		t.Errorf("第二轮不该再拿图片去撞上游（应前置拦下），实际多发了 %d 次", len(p.requests)-calls)
	}
}

// 纯文本这一轮必须照常跑通 —— 能力门不该误伤没有附件的对话。
func TestTextOnlyTurnUnaffected(t *testing.T) {
	resetMemory(t)
	p := &imageRequestProvider{}
	a, sess := newMediaSession(t, p, config.LLMConfig{Model: modelNoVision, Vision: boolPtr(false)})
	if err := a.RunWithMedia(context.Background(), sess.ID, "读一下 README", nil, func(Event) {}); err != nil {
		t.Fatalf("无附件的一轮不该被能力门拦下: %v", err)
	}
	if len(p.requests) != 1 {
		t.Errorf("应正常发一次请求，实际 %d", len(p.requests))
	}
}
