// capability.go 是「模型能不能收下这类附件」的能力门。
//
// 要解决的问题（引入前的症状）
//
//	用户 @ 了一张报错截图 → 附件照发 → 上游 400，body 是
//	"Invalid content type … 'image_url' is not supported" → 归类成
//	KindParse → 界面上显示「数据格式不对（解析失败）」→ 用户去检查自己的
//	PNG 是不是坏了。而此时**图片已经进了历史**，于是这个会话之后每一次
//	请求都被同样拒一次，用户只能新建会话才恢复。
//
//	即：不透明 + 会话被毒化 + 无法自救。
//
// 三道防线（缺一不可）
//
//  1. **声明**：models.yaml 条目可写 vision / video。声明为 false 的，
//     附件根本不该发出去 —— 在发之前拦下，一次请求都不浪费。
//  2. **失败记忆**：未声明时乐观发送（维持既有行为，不给存量配置制造回归）；
//     一旦被拒，记住「这个模型收不了这类附件」，此后前置拦下。
//     这是把「一次不透明失败」变成「至多一次」的机制。
//  3. **回滚**：被拒时把媒体块从历史里摘掉。不摘的话这个会话就废了 ——
//     光记住模型不够，历史里那份 base64 每轮都会被重发、每轮都被拒。
//
// 图片与视频的**处置不同**，这是刻意的：
//
//	图片 → **显式中止**。贴截图提问时，图往往就是问题本身（报错界面、
//	设计稿、UI 细节）。丢掉它继续答，模型会基于残缺信息给出**自信的错误
//	答案**，而用户以为模型看到了图 —— 那比直接失败糟得多。
//	视频 → **提示 + 忽略 + 继续**。视频在提问里通常是补充材料而非唯一依据，
//	跳过它仍能回答文字部分；为它中止整轮不划算。它也从不依赖上游拒绝来
//	判定（能力声明在发之前就知道），所以没有「第一次失败」需要中止。
package agent

import (
	"fmt"
	"strings"
	"sync"

	"codeforge/config"
	"codeforge/pkg/llm"
	"codeforge/pkg/logx"
)

// mediaDecision 是一个附件块的处理决定。
type mediaDecision int

const (
	// mediaSend：正常送给模型。
	mediaSend mediaDecision = iota
	// mediaDropImage：图片收不了 → 上层必须中止本轮（见文件头）。
	mediaDropImage
	// mediaDropVideo：视频收不了 → 上层提示后继续。
	mediaDropVideo
)

// mediaMemory 记住「哪些模型拒收过哪类附件」。
//
// 为什么是进程级内存而不是落盘：这是一条**性能优化**（少撞一次墙），
// 不是正确性依赖 —— 正确性由「回滚历史」保证，落盘那份记忆错了反而会
// 让一个明明支持的模型永久收不到图。重启后重新撞一次 400，代价可接受。
//
// 按「模型 id × 块类型」记而不是只按模型记：模型可能支持图片不支持视频
// （Qwen-VL 一类）。只按模型记会让一次视频失败顺手把图片能力也关掉。
type mediaMemory struct {
	mu sync.RWMutex
	// key 是 `模型 id + "\x00" + 块类型`；用字符串键是为了不必引入 map[string]map 结构。
	rejected map[string]bool
}

var rejectedMedia mediaMemory

func mediaMemoryKey(model, blockType string) string { return model + "\x00" + blockType }

// remembers 报告「该模型此前是否已被确认拒收这类附件」。
func (m *mediaMemory) remembers(model, blockType string) bool {
	if model == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.rejected[mediaMemoryKey(model, blockType)]
}

// note 记下「该模型拒收过这类附件」。
//
// 只在**确认**是媒体被拒时调用（errs.KindUnsupportedMedia），不用于
// 「声明为不支持」—— 声明是持久配置，不需要也不应该被记忆覆盖。
func (m *mediaMemory) note(model, blockType string) {
	if model == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rejected == nil {
		m.rejected = map[string]bool{}
	}
	m.rejected[mediaMemoryKey(model, blockType)] = true
}

// ForgetModel 清掉某个模型的记忆。设置页改了该模型的能力声明后调用 ——
// 用户在那边声明了「支持」，本进程就不该继续拿旧结论拦他。
//
// nil（无声明）**不**触发清理：那不是「支持」，只是「不知道」，
// 拿旧结论拦下是合理的（少撞一次墙）。
func (m *mediaMemory) ForgetModel(model string) bool {
	if model == "" {
		return false
	}
	prefix := model + "\x00"
	m.mu.Lock()
	defer m.mu.Unlock()
	found := false
	for k := range m.rejected {
		if strings.HasPrefix(k, prefix) {
			delete(m.rejected, k)
			found = true
		}
	}
	return found
}

// ForgetMediaCapability 清掉某模型的能力记忆，由设置页在用户把声明改成
// 「支持」时调用。返回是否有东西被清掉（供测试与日志）。
func ForgetMediaCapability(model string) bool {
	return rejectedMedia.ForgetModel(model)
}

// declaredMedia 报告配置对某模态的**显式**声明。
// declared=false 表示模型库条目没写这个字段（= 不知道，不是「不支持」）。
func declaredMedia(cfg config.LLMConfig, video bool) (declared, supported bool) {
	if video {
		if cfg.Video == nil {
			return false, false
		}
		return true, *cfg.Video
	}
	if cfg.Vision == nil {
		return false, false
	}
	return true, *cfg.Vision
}

// blockIsVideo 报告内容块是不是视频（其余媒体模态按图片处理）。
func blockIsVideo(b llm.ContentBlock) bool { return b.Type == llm.BlockVideo }

// mediaVerdict 判定一个媒体块能否送出，并给出处置理由。
//
// 判定顺序（顺序本身就是语义，别调换）：
//
//  1. 显式声明不支持 → 立刻拒。用户在设置页亲手声明的，比任何自动推断可靠。
//  2. 显式声明支持 → 送。**声明优先于记忆**：用户声明支持就是「我确认过了」，
//     不该被上一次失败留下的结论否掉（否则用户在设置页改完声明也不生效）。
//  3. 未声明 + 已记住被拒 → 拒。不必再浪费一次请求去撞同一面墙。
//  4. 未声明 + 未记住 → 送（乐观）。这一条维持引入前的行为，
//     存量配置里那些从没声明过的模型照样能收图。
func mediaVerdict(cfg config.LLMConfig, b llm.ContentBlock) (mediaDecision, string) {
	video := blockIsVideo(b)
	blockType := "image"
	if video {
		blockType = "video"
	}
	name := "图片"
	if video {
		name = "视频"
	}

	declared, supported := declaredMedia(cfg, video)
	if declared {
		if supported {
			return mediaSend, ""
		}
		return dropFor(video), fmt.Sprintf("当前模型「%s」已声明不支持接收%s附件", cfg.Model, name)
	}
	if rejectedMedia.remembers(cfg.Model, blockType) {
		return dropFor(video), fmt.Sprintf("此前已确认模型「%s」不接收%s附件", cfg.Model, name)
	}
	return mediaSend, ""
}

// dropFor 把模态映射到对应的丢弃处置（图片中止、视频继续）。
func dropFor(video bool) mediaDecision {
	if video {
		return mediaDropVideo
	}
	return mediaDropImage
}

// filterMedia 把本轮媒体块过一遍能力门。
//
// 返回可发送的块、被丢掉的图片数、以及**第一条**丢弃理由（用于提示文案；
// 多条理由不逐条展示 —— 「你附的 3 张图和那段视频当前模型都收不了」
// 只需要一句，列全了反而像报错）。
func (a *Agent) filterMedia(cfg config.LLMConfig, media []llm.ContentBlock) (kept []llm.ContentBlock, droppedImages int, reason string) {
	for _, b := range media {
		decision, why := mediaVerdict(cfg, b)
		switch decision {
		case mediaSend:
			kept = append(kept, b)
		case mediaDropImage:
			droppedImages++
			if reason == "" {
				reason = why
			}
		case mediaDropVideo:
			// 视频只是跳过，不计入中止条件，但要让用户知道。
			logx.Infof("跳过视频附件：%s", why)
			if reason == "" {
				reason = why
			}
		}
	}
	return kept, droppedImages, reason
}

// noteRejectedMedia 在上游拒收媒体后记录「哪个模态该被记住」。
//
// 模态的判定是**排除法**，不猜：这一轮实际送出去的是什么，就记什么。
// 只有一种模态时结论唯一；图片与视频同时送出时**先记视频** ——
// 因为视频本来就是「可跳过」的那一类（见文件头），而图片往往是提问的
// 本体、也更容易被上游接受（Gemini 一类同时收图片与视频）。若真正被拒的
// 其实是图片，下一轮视频已不再随行、再撞一次就会把图片也记上 ——
// 代价是多一次失败请求，换来的是**不会猜错**。
func noteRejectedMedia(cfg config.LLMConfig, sent []llm.ContentBlock) {
	hasImage, hasVideo := false, false
	for _, b := range sent {
		if blockIsVideo(b) {
			hasVideo = true
		} else if b.Type == llm.BlockImage {
			hasImage = true
		}
	}
	switch {
	case hasVideo:
		rejectedMedia.note(cfg.Model, "video")
	case hasImage:
		rejectedMedia.note(cfg.Model, "image")
	default:
		// 没有媒体块却收到「拒收媒体」：多半是协议层拒绝（Anthropic 遇到
		// 历史里残留的视频块）。记不着具体模态就不记 —— 硬记会把一个本来
		// 没问题的模型误标成不支持，后果比多撞一次墙大得多。
		logx.Warnf("模型=%s 报拒收媒体但本轮没有媒体块（可能是历史残留），不记入能力记忆", cfg.Model)
	}
}

// onMediaRejected 是「上游拒收媒体」的唯一收敛点。
//
// 放在 runLoopWithLimit 的错误返回处、而不是 RunWithMedia 里，是因为
// **所有入口都会撞上同一件事**，而只有循环层知道「真正发出去的那份请求视图」：
//
//	RunWithMedia   —— 本轮新加的媒体块，已过能力门；
//	Regenerate     —— 用户在历史里贴过图，之后换了纯文本模型；
//	EditAndResend /
//	RerunFrom      —— 同上，断点重试会反复重发同一批历史块；
//	ContinueTurn   —— 同上。
//
// 后四种入口**不新增**媒体块，因此它们没有「能力门」可依赖 ——
// 历史里那些块是在上一个模型下验证过的，换模型后可能正好是收不下的那一种。
// 若只在本轮入口回滚，这条路上会话仍会被毒化（每轮重发、每轮被拒）。
//
// 两件事，缺一不可：
//   - **记住**该模型收不下这类附件（否则下一轮又去撞同一面墙）；
//   - **摘掉**历史里的媒体块（否则摘了记忆，历史那份 base64 每轮仍被重发）。
func (a *Agent) onMediaRejected(sess *Session, view []llm.Message, cfg config.LLMConfig, persist bool, emit Emitter) {
	// 模态判定按「真正送出去的那份」而不是本轮 —— 重新生成这类入口里，
	// 被拒的媒体块可能来自很早以前的一条消息。
	var sent []llm.ContentBlock
	for _, m := range view {
		for _, b := range m.Content {
			if b.Type == llm.BlockImage || b.Type == llm.BlockVideo {
				sent = append(sent, b)
			}
		}
	}
	noteRejectedMedia(cfg, sent)

	sess.mu.Lock()
	removed := 0
	for i := range sess.Messages {
		removed += stripMediaFromMessage(&sess.Messages[i])
	}
	sess.mu.Unlock()
	if removed == 0 {
		return
	}
	logx.Infof("会话=%s 模型=%s 拒收媒体，已从历史摘掉 %d 个块（否则该会话每轮都会被拒）",
		sess.ID, cfg.Model, removed)
	// 必须落盘：不落盘的话用户刷新一下页面，毒化的历史又回来了。
	a.save(sess, persist, emit)
}

// stripMediaFromMessage 把一条消息里的媒体块摘掉，返回摘掉的数量。
//
// 存在的理由：媒体块一旦留在历史里，**这个会话之后每一轮都会被重发**，
// 于是每一轮都被同一个上游错误拒绝。会话就此报废，用户只能新建。
// 摘掉 + 保存历史，是让会话在失败后仍可继续的唯一办法。
//
// 只摘媒体块、保留文本块 —— 用户的问题本身没问题，不该因为附件发不出去
// 就把问题也一起丢掉。
func stripMediaFromMessage(m *llm.Message) int {
	kept := make([]llm.ContentBlock, 0, len(m.Content))
	removed := 0
	for _, b := range m.Content {
		if b.Type == llm.BlockImage || b.Type == llm.BlockVideo {
			removed++
			continue
		}
		kept = append(kept, b)
	}
	if removed == 0 {
		return 0
	}
	m.Content = kept
	return removed
}
