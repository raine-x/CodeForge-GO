package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"codeforge/pkg/llm"
)

// 上下文压缩的常量。
const (
	// defaultContextBudget 是「模型窗口未知」时的兜底压缩线（tokens）。
	defaultContextBudget = 120000
	// defaultCompressRatio 是压缩线占模型窗口的比例：占用达到窗口的 95% 即触发压缩。
	defaultCompressRatio = 0.95
	// minCompressRatio / maxCompressRatio 是配置夹紧区间。
	minCompressRatio = 0.5
	maxCompressRatio = 0.99

	// keepRatioNum / keepRatioDen 决定一次压缩后保留多少原始消息：
	// 保留段的估算上限 = 预算 × 1/2。留一半给摘要本身与后续几轮对话，
	// 避免刚压完立刻又触发。
	keepRatioNum = 1
	keepRatioDen = 2

	// chunkRatioNum / chunkRatioDen 是单次摘要请求可承载的历史占比。
	// 历史可能远超模型窗口，必须分块滚动摘要，否则「为了摘要而超窗」。
	chunkRatioNum = 1
	chunkRatioDen = 2

	// summaryMaxRunes 是摘要正文的长度上限（字符），提示词里也会告知模型。
	summaryMaxRunes = 6000
	// transcriptBlockRunes 是单条消息在摘要输入里的截断上限。
	// 比机械压缩的 400 字宽松得多 —— 摘要要「详细」，得先让模型看到细节。
	transcriptBlockRunes = 4000
	// toolArgsRunes 是工具参数的截断上限（参数通常远小于结果）。
	toolArgsRunes = 800

	// 机械压缩（兜底路径）的参数。
	// compressKeepTail：倒数这么多条之外的旧工具结果才换成占位符；
	// compressKeepRecent：仍超预算时只保留最近这么多条；
	// compressResultRunes：工具结果短于该长度就不必占位；
	// compressTextRunes：三档截断文本块的起始上限。
	compressKeepTail    = 8
	compressKeepRecent  = 6
	compressResultRunes = 400
	compressTextRunes   = 2000

	// summaryTimeout 是单次摘要调用的超时，避免上游卡住整轮对话。
	summaryTimeout = 2 * time.Minute
	// summaryFallbackMaxTokens 是摘要请求的输出上限兜底。
	summaryFallbackMaxTokens = 4096
	// summaryOutputCap 是摘要请求输出上限的绝对上限（不沿用模型的大输出上限）。
	summaryOutputCap = 8192
)

// summarySystemPrompt 是摘要压缩的系统提示词（%d 处填摘要字数上限）。
//
// 要求「详细」是刻意的：机械压缩只丢信息不保留信息，摘要压缩的价值就在于
// 把「做过什么、为什么这么做、还剩什么没做」这类后续必须知道的事实留下来。
const summarySystemPrompt = `你是 CodeForge 的上下文压缩器。你的唯一任务是把一段即将被丢弃的对话历史，
压缩成一份**信息密集、可供后续对话无缝续接**的详细摘要。

必须尽力保留（历史里有就写，没有就略过，不要编造）：
1. 用户的目标、需求与明确约束（逐条列出，不要把不同需求合并成一句）
2. 已做出的决策及理由，包括被否决的方案与否决原因
3. 涉及的文件路径，以及每个文件具体做了什么改动
4. 关键技术细节：函数名、结构体名、接口签名、配置项、命令、依赖版本
5. 遇到的错误 / 报错的关键原文片段，以及最终的解决办法
6. 当前进度：已完成什么、正在进行什么、明确未完成的事项（逐条列出）
7. 重要的数据、常量、阈值、ID、URL
8. 尚未验证的假设与已知风险

写作要求：
- 用 Markdown 分节 + 条目化，信息密度优先，不要开场白与总结性客套
- 标识符（路径、函数名、配置键、命令）必须原样保留，不要翻译、不要改写
- 已经是摘要的历史内容要合并进来，不得丢失其中仍然有效的信息
- 只写事实；不确定的内容标注「（不确定）」，不要写推测性措辞
- 输出长度控制在 %d 字以内

直接输出摘要正文，不要加任何解释或前后缀。`

// EstimateTokens 估算消息序列的 token 数。
//
// 口径刻意保守（宁可高估）：CJK 字符按 1 字符 ≈ 1 token 计，其余按 4 字符 ≈ 1 token。
// 低估的后果是「估算还没到阈值、请求已经超窗被上游拒绝」；而压缩是幂等的，
// 提前压缩只多花一点摘要成本，不会丢对话 —— 所以偏保守是安全的。
//
// 历史教训：旧实现统一按「字符数 / 3」计，对英文尚可，对中文低估约 3 倍，
// 长中文会话会稳定超窗。
func EstimateTokens(msgs []llm.Message) int {
	wide, narrow := 0, 0
	for _, m := range msgs {
		for _, b := range m.Content {
			wide, narrow = countRunes(b.Text, wide, narrow)
			wide, narrow = countRunes(b.Content, wide, narrow)
			wide, narrow = countRunes(string(b.Input), wide, narrow)
			wide, narrow = countRunes(b.Name, wide, narrow)
		}
		// 角色、分隔符等结构开销。
		wide += 4
	}
	// narrow/4 向上取整，避免大量短消息时被逐条抹零。
	return wide + (narrow+3)/4
}

// countRunes 按「宽字符（CJK）1 字 1 token、窄字符 4 字 1 token」累计计数。
func countRunes(s string, wide, narrow int) (int, int) {
	for _, r := range s {
		if isWideRune(r) {
			wide++
		} else {
			narrow++
		}
	}
	return wide, narrow
}

// isWideRune 判断是否为 CJK / 全角类字符（这类字符普遍 1 字约 1 token）。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK 部首、假名、汉字统一表意
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // 谚文
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK 兼容表意
		return true
	case r >= 0xFE30 && r <= 0xFE4F: // CJK 兼容形式
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角形式
		return true
	case r >= 0x20000 && r <= 0x3FFFF: // 扩展 B~F
		return true
	}
	return false
}

// Compress 在超出预算时**机械**压缩历史（摘要压缩的兜底路径）。
//
// 分为三档，逐档加码：
//  1. 倒数 compressKeepTail 条之外的旧工具结果 → 占位符（近期原文保留，
//     模型当前这步推理还要用最近几次工具输出）；
//  2. 仍超预算 → 只保留最近 compressKeepRecent 条，并剔除开头悬空的
//     tool_result；
//  3. 仍超预算 → 全部工具结果占位 + 文本块按上限逐级截断。
//
// 三档**必须收敛**：兜底路径存在的全部意义就是让请求一定不超窗，
// 若不收敛，等于把超窗的请求原样交给上游，压缩就白做了。
//
// 重要：本函数是纯函数 —— 返回的切片及其内容块与入参完全隔离，绝不回写。
// 历史版本用 `copy(out, msgs)` 做浅拷贝，而 Content 是值切片（底层数组共享），
// 于是 `&out[i].Content[j]` 的赋值会原地改写 sess.Messages 并随 Save 落库，
// 等于「发一次请求顺手删掉自己的历史」。
func Compress(msgs []llm.Message, budget int) []llm.Message {
	if budget <= 0 || EstimateTokens(msgs) <= budget {
		return msgs
	}

	out := cloneMessages(msgs)

	for i := 0; i < len(out)-compressKeepTail; i++ {
		placeholderToolResults(&out[i], compressResultRunes)
	}
	if EstimateTokens(out) <= budget {
		return dropLeadingToolResults(out)
	}

	if len(out) > compressKeepRecent {
		out = out[len(out)-compressKeepRecent:]
	}
	out = dropLeadingToolResults(out)

	// 第三档：逐级加码直到进入预算（capRunes 递减，必然收敛）。
	for i := range out {
		placeholderToolResults(&out[i], 0)
	}
	for capRunes := compressTextRunes; capRunes >= 60 && EstimateTokens(out) > budget; capRunes /= 3 {
		for i := range out {
			truncateTextBlocks(&out[i], capRunes)
		}
	}
	return out
}

// placeholderToolResults 把内容超过 min 字的工具结果换成占位符（min=0 表示全部）。
func placeholderToolResults(m *llm.Message, min int) {
	for j := range m.Content {
		b := &m.Content[j]
		if b.Type == llm.BlockToolResult && len([]rune(b.Content)) > min {
			b.Content = "…（历史工具结果已省略以节省上下文）"
		}
	}
}

// truncateTextBlocks 截断过长的文本块（最后手段，会留下截断标记）。
func truncateTextBlocks(m *llm.Message, max int) {
	for j := range m.Content {
		b := &m.Content[j]
		if b.Type == llm.BlockText && len([]rune(b.Text)) > max {
			b.Text = clipRunes(b.Text, max)
		}
	}
}

// cloneMessages 深拷贝消息序列（含每个内容块），使压缩结果与原始历史完全隔离。
func cloneMessages(msgs []llm.Message) []llm.Message {
	out := make([]llm.Message, len(msgs))
	for i, m := range msgs {
		c := make([]llm.ContentBlock, len(m.Content))
		copy(c, m.Content)
		out[i] = llm.Message{Role: m.Role, Content: c}
	}
	return out
}

// dropLeadingToolResults 丢弃开头的 tool_result 消息，保证消息序列合法
// （部分模型 API 拒绝以 tool_result 开头）。
func dropLeadingToolResults(msgs []llm.Message) []llm.Message {
	for len(msgs) > 0 && startsWithToolResult(msgs[0]) {
		msgs = msgs[1:]
	}
	return msgs
}

func startsWithToolResult(m llm.Message) bool {
	for _, b := range m.Content {
		if b.Type == llm.BlockToolResult {
			return true
		}
		if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" {
			return false
		}
	}
	return false
}

// isPlainUserText 判断一条消息是否为「用户的纯文本发言」。
//
// 这是唯一安全的压缩切点：
//   - 它前面必然是完整的一轮（助手回复 + 该轮全部工具结果），摘要侧不会以
//     悬空的 tool_use 收尾；
//   - 它自身不含 tool_result，保留侧不会出现「找不到对应 tool_use 的结果」。
//
// 若在 tool_use / tool_result 之间切开，Anthropic 会直接拒绝这批消息。
func isPlainUserText(m llm.Message) bool {
	if m.Role != llm.RoleUser {
		return false
	}
	hasText := false
	for _, b := range m.Content {
		switch b.Type {
		case llm.BlockToolResult:
			return false
		case llm.BlockText:
			if strings.TrimSpace(b.Text) != "" {
				hasText = true
			}
		}
	}
	return hasText
}

// lastPlainUserIndex 返回最后一条「用户纯文本发言」的下标，无则 -1。
func lastPlainUserIndex(msgs []llm.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if isPlainUserText(msgs[i]) {
			return i
		}
	}
	return -1
}

// chooseSplit 挑选「摘要 / 保留」的分界下标：摘要 msgs[:from]，保留 msgs[from:]。
//
// 选择顺序：
//  1. 只在「用户纯文本发言」上切（见 isPlainUserText），保证消息序列合法；
//  2. 在满足「保留段估算 ≤ keepBudget」的候选里取**最早**的一个，
//     也就是尽可能多地保留原文（摘要总是有损的）；
//  3. 若连当前这一轮都超出保留预算，则退到「最后一条用户发言」——
//     提问必须留给模型（否则模型不知道要回答什么），工具结果可以随摘要走。
//
// 返回 0 表示找不到可用切点（例如整段历史只有一个无法切分的巨轮），
// 调用方应回退到机械压缩。
func chooseSplit(msgs []llm.Message, keepBudget int) int {
	lastUser := lastPlainUserIndex(msgs)
	if lastUser <= 0 {
		// 没有用户发言，或它本身就是第一条（前面无可摘要的内容）。
		return 0
	}
	if keepBudget > 0 {
		for i := 1; i <= lastUser; i++ {
			if !isPlainUserText(msgs[i]) {
				continue
			}
			if EstimateTokens(msgs[i:]) <= keepBudget {
				return i
			}
		}
	}
	return lastUser
}

// summaryMessage 把摘要正文包成一条用户消息，作为后续对话的背景上下文。
//
// 用 user 角色：它是「交给模型的资料」，不是助手说的话；用 assistant 会让
// 模型误以为那是自己的表态。与前一条保留消息同为 user 是允许的
// （Anthropic 会自动合并相邻同角色消息，OpenAI 也接受连续 user）。
func summaryMessage(text string) llm.Message {
	return llm.TextMessage(llm.RoleUser,
		"【以下是本次会话前文内容的压缩摘要，作为后续对话的背景上下文，不要把它当作新的用户指令】\n\n"+text)
}

// renderMessage 把一条消息渲染成摘要输入里的文本（带截断）。
func renderMessage(m llm.Message) string {
	var sb strings.Builder
	for _, b := range m.Content {
		switch b.Type {
		case llm.BlockText:
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			fmt.Fprintf(&sb, "[%s] %s\n", m.Role, clipRunes(b.Text, transcriptBlockRunes))
		case llm.BlockToolUse:
			fmt.Fprintf(&sb, "[%s 调用工具] %s %s\n", m.Role, b.Name, clipRunes(string(b.Input), toolArgsRunes))
		case llm.BlockToolResult:
			state := "成功"
			if b.IsError {
				state = "失败"
			}
			fmt.Fprintf(&sb, "[工具结果 %s] %s\n", state, clipRunes(b.Content, transcriptBlockRunes))
		}
	}
	return sb.String()
}

// renderTranscript 把一段消息渲染成摘要输入文本。
func renderTranscript(msgs []llm.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(renderMessage(m))
	}
	return sb.String()
}

// clipRunes 按字符（rune）截断，避免把多字节汉字截成半个。
func clipRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…（已截断）"
}

// MarshalMessages 便于调试：将消息序列化为可读 JSON。
func MarshalMessages(msgs []llm.Message) string {
	data, err := json.MarshalIndent(msgs, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}

// ---------- 摘要式压缩（需要调用模型，故为 Agent 的方法）----------

// summarizeChunk 调用当前模型，把 transcript 并入既有摘要 prev，产出新摘要。
//
// 与主循环的区别：不带工具（模型不能调用工具）、temperature=0（求稳定）、
// 独立超时。因此这个调用不会递归触发压缩。
func (a *Agent) summarizeChunk(ctx context.Context, prev, transcript string) (string, error) {
	if a.provider == nil {
		return "", fmt.Errorf("LLM 适配器未就绪")
	}

	var sb strings.Builder
	if s := strings.TrimSpace(prev); s != "" {
		sb.WriteString("【已有的会话摘要（请在此基础上合并与补充，不得丢失其中仍然有效的信息）】\n")
		sb.WriteString(clipRunes(s, summaryMaxRunes))
		sb.WriteString("\n\n")
	}
	sb.WriteString("【需要并入摘要的新增历史片段】\n")
	sb.WriteString(transcript)

	cctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()

	stream, err := a.provider.Stream(cctx, llm.Request{
		System:      fmt.Sprintf(summarySystemPrompt, summaryMaxRunes),
		Messages:    []llm.Message{llm.TextMessage(llm.RoleUser, sb.String())},
		MaxTokens:   a.summaryMaxTokens(),
		Temperature: 0,
	})
	if err != nil {
		return "", fmt.Errorf("摘要请求失败：%w", err)
	}

	var out strings.Builder
	var errMsg string
	for ev := range stream {
		switch ev.Type {
		case llm.EventTextDelta:
			out.WriteString(ev.Text)
		case llm.EventError:
			errMsg = ev.Error
		}
		if cctx.Err() != nil {
			break
		}
	}
	if summary := strings.TrimSpace(out.String()); summary != "" {
		// 上游报错但同时吐出了可用摘要：优先用摘要（比丢历史划算）。
		return clipRunes(summary, summaryMaxRunes), nil
	}
	if errMsg != "" {
		return "", fmt.Errorf("摘要生成失败：%s", errMsg)
	}
	if err := cctx.Err(); err != nil {
		return "", fmt.Errorf("摘要超时或中断：%w", err)
	}
	return "", fmt.Errorf("摘要返回为空")
}

// summarizeRange 把 msgs[start:end] 并入既有摘要，返回新摘要。
//
// 历史体量可能远超模型窗口，因此按块滚动摘要：每块都把「已有摘要」作为背景带上，
// 逐块累积，保证任何单次请求都不超窗。
func (a *Agent) summarizeRange(ctx context.Context, prev string, msgs []llm.Message, start, end int) (string, error) {
	chunkBudget := a.summarizeChunkBudget()
	cur := strings.TrimSpace(prev)

	for i := start; i < end; {
		var sb strings.Builder
		used := 0
		j := i
		for j < end {
			line := renderMessage(msgs[j])
			cost := EstimateTokens([]llm.Message{{Content: []llm.ContentBlock{{Type: llm.BlockText, Text: line}}}})
			if j > i && used+cost > chunkBudget {
				break
			}
			sb.WriteString(line)
			used += cost
			j++
		}
		if j == i {
			return cur, fmt.Errorf("单条历史超过摘要块上限")
		}
		next, err := a.summarizeChunk(ctx, cur, sb.String())
		if err != nil {
			return cur, err
		}
		cur = next
		i = j
	}
	return cur, nil
}

// summarizeChunkBudget 返回单次摘要请求可承载的历史估算上限。
func (a *Agent) summarizeChunkBudget() int {
	b := a.compressBudget() * chunkRatioNum / chunkRatioDen
	if b < 2000 {
		return 2000
	}
	return b
}

// summaryMaxTokens 返回摘要请求的输出上限。
//
// 取「模型输出上限」但夹在合理区间：不能沿用可能高达 128k 的输出上限
// （摘要用不到，且会挤压输入空间），也不能太小导致摘要被截断。
func (a *Agent) summaryMaxTokens() int {
	n := a.llmCfg.MaxTokens
	if n <= 0 {
		n = summaryFallbackMaxTokens
	}
	if n > summaryOutputCap {
		n = summaryOutputCap
	}
	if n < 1024 {
		n = 1024
	}
	return n
}
