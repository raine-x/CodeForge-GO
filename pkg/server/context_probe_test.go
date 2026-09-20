package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

// 上下文自动探测：从最大档往下试，只有「确实因长度被拒」才降档。

// 让请求体不至于真的灌满几 MB：探测逻辑测的是判定与降档行为，
// 上游侧用「声明的 token 上限」按估算值裁决即可。
func fakeUpstream(t *testing.T, declaredLimit int, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &req)
		got := 0
		if len(req.Messages) > 0 {
			got = agent.EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, req.Messages[0].Content)})
		}
		switch mode {
		case "auth": // 鉴权失败一类：与长度无关
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
			return
		case "silent200": // 200 流里带长度错误（不少网关这样回）
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `data: {"error":{"message":"This model's maximum context length is %d tokens, request too long"}}`,
				declaredLimit)
			return
		}
		if got > declaredLimit {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintf(w, `{"error":{"message":"This model's maximum context length is %d tokens. However, you requested %d tokens"}}`,
				declaredLimit, got)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"))
	}))
}

func probeAgainst(t *testing.T, mode string, declaredLimit int) probeOutcome {
	t.Helper()
	ts := fakeUpstream(t, declaredLimit, mode)
	t.Cleanup(ts.Close)
	return probeContextWindow(t.Context(), ts.Client(), ts.URL+"/chat/completions",
		map[string]string{"Content-Type": "application/json"}, "openai", "fake-model")
}

func TestProbeContextWindowDescendsToFirstAcceptedLevel(t *testing.T) {
	// 上游真实上限 100k：131072 档（填到 ~127k）应被拒，65536 档（~63.6k）应被接受。
	out := probeAgainst(t, "normal", 100000)
	if out.CtxIn != 65536 {
		t.Fatalf("应定档在 65536，实际 %d，逐档：%+v", out.CtxIn, out.Steps)
	}
	if len(out.Steps) == 0 {
		t.Fatal("必须带回逐档结果，前端要靠它说明依据")
	}
	last := out.Steps[len(out.Steps)-1]
	if !last.Accepted || last.Want != 65536 {
		t.Errorf("最后一档应为被接受的 65536：%+v", last)
	}
	for _, s := range out.Steps[:len(out.Steps)-1] {
		if s.Accepted {
			t.Errorf("降档过程中出现了被接受的档位（说明提前收尾了）：%+v", s)
		}
		if s.Reason == "" {
			t.Errorf("被拒的档位要带原因：%+v", s)
		}
	}
	if out.Note != "" {
		t.Errorf("探测成功时不该带 Note：%q", out.Note)
	}
}

// 与长度无关的错误（鉴权失败 / 模型名错）必须**立刻中止**，
// 一路降档到底只会报出一个荒谬的小窗口。
func TestProbeAbortsOnNonLengthError(t *testing.T) {
	out := probeAgainst(t, "auth", 100000)
	if out.CtxIn != 0 {
		t.Errorf("非长度错误时不该给出任何窗口值：%+v", out)
	}
	if len(out.Steps) != 1 {
		t.Errorf("应只试最大档就停，实际 %d 档：%+v", len(out.Steps), out.Steps)
	}
	if !strings.Contains(out.Note, "invalid api key") {
		t.Errorf("Note 要带上游原始原因：%q", out.Note)
	}
}

// 200 + 流内错误：不能因为状态码好看就判定「这一档放得下」。
func TestProbeTreatsInStreamLengthErrorAsRejected(t *testing.T) {
	out := probeAgainst(t, "silent200", 100000)
	if out.CtxIn != ctxProbeFloor {
		t.Errorf("每一档都被拒时应落到哨兵值，实际定档 %d", out.CtxIn)
	}
	if !strings.Contains(out.Note, "最小档") {
		t.Errorf("要说明是「连最小档都被拒」：%q", out.Note)
	}
	for _, s := range out.Steps {
		if s.Accepted {
			t.Errorf("流内长度错误被判成了放得下：%+v", s)
		}
	}
	if len(out.Steps) != len(ctxProbeLadder) {
		t.Errorf("该模式每档都被拒，应逐档试完：%d/%d", len(out.Steps), len(ctxProbeLadder))
	}
}

func TestFillerForHitsTargetSize(t *testing.T) {
	for _, want := range []int{16384, 65536, 262144} {
		s := fillerFor(want)
		got := agent.EstimateTokens([]llm.Message{llm.TextMessage(llm.RoleUser, s)})
		if got > want {
			t.Errorf("filler(%d) 估算 %d，超过档位本身", want, got)
		}
		// 填充比例 97%：允许分词与估算的小幅偏差，但不能偏太多（偏少就等于没探到那一档）。
		if got*100 < want*88 {
			t.Errorf("filler(%d) 只填到 %d（%.0f%%），太少", want, got, float64(got)*100/float64(want))
		}
	}
}

func TestLooksLikeLengthLimitOnlyOnRealSignals(t *testing.T) {
	yes := []string{
		"This model's maximum context length is 8192 tokens",
		`{"error":{"message":"prompt is too long: 210000 tokens > 128000 maximum"}}`,
		"输入长度超过模型限制",
		"Request body too large",
	}
	for _, s := range yes {
		if !looksLikeLengthLimit(s) {
			t.Errorf("应识别为长度受限：%q", s)
		}
	}
	no := []string{
		"invalid api key",
		"model not found: gpt-9",
		"rate limit exceeded, retry later", // 429 属限流，不是窗口不够
	}
	for _, s := range no {
		if looksLikeLengthLimit(s) {
			t.Errorf("不该识别为长度受限：%q", s)
		}
	}
}
