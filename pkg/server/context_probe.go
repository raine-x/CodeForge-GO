// context_probe.go 实现「模型输入上下文自动探测」：从最大候选档开始发一份
// 撑到该档体量的请求，被拒就降到下一档，直到找到第一个被接受的档位。
//
// 为什么值得做：新模型的 ctx_in 只能手填，填大了会在长任务里超窗被上游拒，
// 填小了则白白浪费窗口（压缩线是按 ctx_in 算的）。用户给的信息常常不准，
// 最可靠的证据就是上游自己的接受边界。
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

// ctxProbeLadder 是候选输入窗口（tokens，从大到小）。
// 只取市面上真实存在的规格：1M / 512K / 256K / 200K(Claude) / 128K / 64K / 32K / 16K / 8K。
var ctxProbeLadder = []int{1048576, 524288, 262144, 200000, 131072, 65536, 32768, 16384, 8192}

const (
	// probeFillRatioPct 是每档的实际填充比例。
	//
	// 贴着 100% 填会误判：我们的 token 估算与上游分词器必然有偏差，
	// 一个「其实放得下」的档位可能因为多算了几千 token 而被拒，
	// 于是探测结果凭空少一档。留 3% 余量，宁可少报一档也不虚报。
	probeFillRatioPct = 97
	// probeAttemptTimeout 是单档的上限：最大档要上传约 4MB 文本，
	// 而「被接受」意味着上游真的把这份输入吃进去（prefill），慢网关会到几十秒。
	probeAttemptTimeout = 90 * time.Second
	// probeTotalTimeout 是整轮探测的总预算，防止一路卡死在超时上。
	probeTotalTimeout = 4 * time.Minute
	// ctxProbeFloor 是「逐档试完没有任何一档被接受」时的哨兵值
	//（0 留给「遇到与长度无关的错误而中止」，两者在前端是不同的说法）。
	ctxProbeFloor = -1
	// probeHeadBytes 是每档回读的上游响应头字节数：不少网关把长度错误塞在
	// 200 的流式响应体里，只看状态码会把它当成「这一档放得下」。
	probeHeadBytes = 4096
)

// probeStep 是一档探测的结果，原样回给前端 —— 用户要能看出为什么停在某一档。
type probeStep struct {
	Want     int    `json:"want"`
	Accepted bool   `json:"accepted"`
	Status   int    `json:"status,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// probeOutcome 是整轮探测的结论。
type probeOutcome struct {
	CtxIn int         `json:"ctx_in"`
	Steps []probeStep `json:"steps"`
	Note  string      `json:"note,omitempty"`
}

// lengthErrorMarkers 是「因输入过长被拒」的判据关键词。
//
// 必须要求明确信号：把 400 参数错误、404 模型名错、鉴权失败都当成「窗口不够大」
// 一路降档，最后会报出一个完全虚假的上下文值 —— 宁可不给，也不能给错。
var lengthErrorMarkers = []string{
	"context length", "maximum context", "context_length", "context window",
	"prompt is too long", "input is too long", "request is too large", "too large",
	"too many tokens", "max token", "token limit", "exceeds the", "length exceeded",
	"超过", "过长", "超出",
}

// looksLikeLengthLimit 判断上游报错是否属于「输入超出模型上下文」。
func looksLikeLengthLimit(text string) bool {
	low := strings.ToLower(text)
	for _, m := range lengthErrorMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// fillerFor 构造一份估算体量约等于 want 的正文。
//
// 用 agent.EstimateTokens 而不是自己拍系数：探测出来的窗口接下来就是喂给
// 压缩阈值算账的，两边必须同一口径，否则「探到 128K」实际能用的却是 90K。
func fillerFor(want int) string {
	target := want * probeFillRatioPct / 100
	// 一句 60 余字符 ≈ 15 token（英文按 4 字符 ≈ 1 token）。先按比例铺，
	// 再用真实估算值回校一次，避免把「估算」当成「上游分词」。
	unit := "The quick brown fox jumps over the lazy dog while probing context. "
	tokensPerUnit := agent.EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, unit)})
	if tokensPerUnit <= 0 {
		tokensPerUnit = 1
	}
	n := target / tokensPerUnit
	if n < 1 {
		n = 1
	}
	var sb strings.Builder
	sb.Grow(n * len(unit))
	for i := 0; i < n; i++ {
		sb.WriteString(unit)
	}
	s := sb.String()

	// 回校：估算值偏大就按比例裁掉尾部，偏小就补一段。
	for round := 0; round < 3; round++ {
		measured := agent.EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, s)})
		switch {
		case measured > target:
			cut := int(float64(len(s)) * float64(target) / float64(measured))
			if cut < 1 {
				return ""
			}
			s = s[:cut]
		case measured*100 < target*probeFillRatioPct:
			s += strings.Repeat(unit, (target-measured)/tokensPerUnit+1)
		default:
			return s
		}
	}
	return s
}

// probePayload 按协议拼一次「只有输入、几乎没有输出」的请求体。
// max_tokens=1：探测要的是「上游肯不肯收下这份输入」，不是它生成多少。
func probePayload(protocol, model, filler string) []byte {
	if protocol == "anthropic" {
		body, _ := json.Marshal(map[string]any{
			"model": model, "stream": true, "max_tokens": 1,
			"messages": []map[string]any{{"role": "user", "content": filler + "\n只回复 OK"}},
		})
		return body
	}
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": true, "max_tokens": 1,
		"messages": []map[string]any{{"role": "user", "content": filler + "\n只回复 OK"}},
	})
	return body
}

// probeOnce 探测单一档位。
//
// 返回 accepted=true 表示这一档上游收下并开始返回；否则 reason 说明被拒原因，
// tooLong=true 表示这是「输入过长」，可以继续降档。
func probeOnce(ctx context.Context, client *http.Client, apiURL string, headers map[string]string,
	protocol, model string, want int) (accepted bool, status int, reason string, tooLong bool) {
	actx, cancel := context.WithTimeout(ctx, probeAttemptTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(actx, http.MethodPost, apiURL,
		bytes.NewReader(probePayload(protocol, model, fillerFor(want))))
	if err != nil {
		return false, 0, err.Error(), false
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		// 超时/断连不能算「这一档放不下」：可能是网络抖动，也可能是超大请求被网关掐断。
		// 判成 tooLong 会一路降到 8K，报出一个荒谬的窗口；这里直接中止整轮探测。
		return false, 0, "请求未得到响应：" + err.Error(), false
	}
	defer resp.Body.Close()

	head, _ := io.ReadAll(io.LimitReader(resp.Body, probeHeadBytes))
	text := string(head)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 2xx 但流里带 error 的网关很常见（长度错误尤其喜欢这样回），别当成放得下。
		if looksLikeLengthLimit(text) && strings.Contains(text, "\"error\"") {
			return false, resp.StatusCode, clipText(strings.TrimSpace(text), 240), true
		}
		return true, resp.StatusCode, "", false
	}
	if looksLikeLengthLimit(text) {
		return false, resp.StatusCode, clipText(strings.TrimSpace(text), 240), true
	}
	return false, resp.StatusCode, "HTTP " + strconv.Itoa(resp.StatusCode) + " " +
		clipText(strings.TrimSpace(text), 240), false
}

// probeContextWindow 从最大档往下探，返回第一个被接受的档位。
//
// 逐档下调而不是二分：被拒的档位上游不会计费（请求在生成前就被挡下），
// 而成功的档位只发生一次 —— 从大到小线性下降正好把「花钱的那一次」压到最少。
func probeContextWindow(ctx context.Context, client *http.Client, apiURL string,
	headers map[string]string, protocol, model string) probeOutcome {
	pctx, cancel := context.WithTimeout(ctx, probeTotalTimeout)
	defer cancel()

	out := probeOutcome{CtxIn: ctxProbeFloor}
	for _, want := range ctxProbeLadder {
		if err := pctx.Err(); err != nil {
			out.Note = "探测超时或被取消，未能定档"
			return out
		}
		accepted, status, reason, tooLong := probeOnce(pctx, client, apiURL, headers, protocol, model, want)
		out.Steps = append(out.Steps, probeStep{Want: want, Accepted: accepted, Status: status, Reason: reason})
		if accepted {
			out.CtxIn = want
			return out
		}
		if !tooLong {
			// 不是长度问题（鉴权失败 / 模型名错 / 网关异常）：继续降档只会得出假结论，立刻停。
			out.Note = reason
			out.CtxIn = 0
			return out
		}
	}
	out.Note = "连最小档都被拒，未探测到可用上下文"
	return out
}
