package server

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"codeforge/config"
	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
	"codeforge/pkg/security"
	"codeforge/pkg/store"
	"codeforge/pkg/tools"
	"codeforge/pkg/tools/builtin"
)

// 端到端测试使用的文件内容标记。
const (
	e2eMarkerOld = "MARKER-ALPHA-111"
	e2eMarkerNew = "MARKER-BETA-222"
)

// TestE2ELiveLLM 用真实 LLM 端点跑通完整链路：
//
//	用户消息 → ReAct 循环 → 工具调用（只读自动放行）→ 工具结果回填
//	→ 写操作触发 HITL 审批 → 批准 → 文件真实修改 → 会话持久化
//
// 默认跳过；设置环境变量后运行：
//
//	CODEFORGE_E2E=1 LLM_API_KEY=sk-xxx go test ./pkg/server/ -run TestE2ELiveLLM -v -timeout 600s
func TestE2ELiveLLM(t *testing.T) {
	if os.Getenv("CODEFORGE_E2E") == "" {
		t.Skip("未设置 CODEFORGE_E2E=1，跳过真实 LLM 端到端测试")
	}
	apiKey := strings.TrimSpace(os.Getenv("LLM_API_KEY"))
	if apiKey == "" {
		t.Skip("未设置 LLM_API_KEY，跳过真实 LLM 端到端测试")
	}
	baseURL := envOr("LLM_BASE_URL", "https://api.tokenrouter.com/v1")
	model := envOr("LLM_MODEL", "z-ai/glm-5.3-free")

	// ---- 准备隔离的工作目录与目标文件
	dir := t.TempDir()
	target := filepath.Join(dir, "hello.txt")
	original := "第一行\n" + e2eMarkerOld + "\n最后一行\n"
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}

	// ---- 组装服务（provider = custom，指向真实 OpenAI 兼容端点）
	cfg := config.Default()
	cfg.Agent.WorkDir = dir
	cfg.Agent.MaxSteps = 8
	cfg.DataDir = filepath.Join(dir, ".codeforge")
	cfg.AuditLog = filepath.Join(dir, ".codeforge", "audit.jsonl")
	cfg.LLM = config.LLMConfig{
		Provider:    "custom",
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		MaxTokens:   1024,
		Temperature: 0.2,
	}

	provider, err := llm.NewProvider(cfg.LLM)
	if err != nil {
		t.Fatalf("构造 LLM 适配器失败: %v", err)
	}

	registry := tools.NewRegistry()
	fsys := builtin.NewFS(dir)
	builtin.RegisterFS(registry, fsys)
	builtin.RegisterTerminal(registry, fsys)
	builtin.RegisterSearch(registry, fsys)

	policy := security.NewPolicy(cfg.Security)
	audit, err := security.NewAuditLogger(cfg.AuditLog)
	if err != nil {
		t.Fatalf("初始化审计日志失败: %v", err)
	}
	t.Cleanup(func() { _ = audit.Close() })

	executor := tools.NewExecutor(registry, policy, audit, nil, 180*time.Second, 64*1024)
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	history := agent.NewHistory(st)
	if _, err := history.Create(dir, ""); err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	ag := agent.New(cfg.Agent, cfg.LLM, provider, executor, history, dir)

	ts := httptest.NewServer(New(cfg, ag, executor, registry, fsys).Routes())
	defer ts.Close()

	// ---- 建立带 Cookie 鉴权的 WebSocket 连接
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("获取鉴权 Cookie 失败: %v", err)
	}
	_ = resp.Body.Close()

	dialer := websocket.Dialer{Jar: jar}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatalf("WebSocket 握手失败: %v", err)
	}
	defer conn.Close()

	var ready map[string]any
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	if err := conn.ReadJSON(&ready); err != nil {
		t.Fatalf("读取 ready 帧失败: %v", err)
	}
	if ready["type"] != "ready" {
		t.Fatalf("首帧期望 ready，实际 %v", ready["type"])
	}
	t.Logf("已连接：平台=%v 工具数=%v", ready["config"].(map[string]any)["platform"], len(ready["tools"].([]any)))

	// run 发送一条用户消息并收集事件，直到 idle。
	run := func(prompt string, approve bool) []map[string]any {
		t.Helper()
		if err := conn.WriteJSON(map[string]any{"type": "user_message", "text": prompt}); err != nil {
			t.Fatalf("发送消息失败: %v", err)
		}
		var events []map[string]any
		deadline := time.Now().Add(240 * time.Second)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(240 * time.Second))
			var ev map[string]any
			if err := conn.ReadJSON(&ev); err != nil {
				t.Fatalf("读取事件失败: %v", err)
			}
			events = append(events, ev)
			switch ev["type"] {
			case "hitl_request":
				id, _ := ev["approval_id"].(string)
				t.Logf("  ↳ HITL 审批请求：tool=%v action=%v", ev["tool"], ev["action"])
				if err := conn.WriteJSON(map[string]any{
					"type": "hitl_decision", "approval_id": id, "approved": approve,
				}); err != nil {
					t.Fatalf("发送审批决策失败: %v", err)
				}
			case "error":
				t.Fatalf("Agent 返回错误: %v", ev["error"])
			case "idle":
				return events
			}
		}
		t.Fatal("等待 idle 超时")
		return nil
	}

	// ================= 场景 1：只读工具应自动放行 =================
	t.Log("场景 1：读取文件（期望 read_file 自动放行）")
	events1 := run("请使用 read_file 工具读取 hello.txt 的内容，然后用中文告诉我这个文件里写了什么。", false)

	text1 := collectText(events1)
	calls1 := filterType(events1, "tool_call")
	results1 := filterType(events1, "tool_result")
	t.Logf("  事件类型统计: %s", typeHistogram(events1))
	t.Logf("  工具调用: %v", toolNames(calls1))
	t.Logf("  最终回复: %s", truncate(text1, 200))

	if len(calls1) == 0 {
		t.Fatalf("未产生任何工具调用，模型可能忽略了工具。事件类型: %s", typeHistogram(events1))
	}
	if len(results1) == 0 {
		t.Fatal("有工具调用但没有工具结果")
	}
	if !containsAny(text1, e2eMarkerOld, "MARKER") && !resultsContain(results1, e2eMarkerOld) {
		t.Errorf("回复与工具结果中均未出现文件标记 %s，可能未真正读取文件", e2eMarkerOld)
	}
	for _, ev := range calls1 {
		if ev["decision"] == "ask" && ev["tool_name"] == "read_file" {
			t.Errorf("只读工具 read_file 不应触发审批，实际 decision=%v", ev["decision"])
		}
	}

	// ================= 场景 2：写操作必须走 HITL 审批 =================
	t.Log("场景 2：修改文件（期望触发 HITL 审批 → 批准 → 文件真实变更）")
	events2 := run("请使用 edit_file 工具，把 hello.txt 里的 "+e2eMarkerOld+" 修改为 "+e2eMarkerNew+"。", true)

	hitls := filterType(events2, "hitl_request")
	calls2 := filterType(events2, "tool_call")
	t.Logf("  事件类型统计: %s", typeHistogram(events2))
	t.Logf("  工具调用: %v", toolNames(calls2))
	t.Logf("  审批请求数: %d", len(hitls))
	if len(hitls) == 0 {
		t.Fatalf("写操作未触发 HITL 审批，安全策略可能未生效。事件类型: %s", typeHistogram(events2))
	}

	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取文件失败: %v", err)
	}
	t.Logf("  文件内容变更后: %q", string(after))

	if !strings.Contains(string(after), e2eMarkerNew) {
		t.Errorf("批准后文件未被修改：期望包含 %s，实际内容 %q", e2eMarkerNew, string(after))
	}
	if strings.Contains(string(after), e2eMarkerOld) {
		t.Errorf("旧标记 %s 仍存在，替换未完成", e2eMarkerOld)
	}

	// ---- 审计日志应同时记录 allow 与 ask 决策
	auditData, err := os.ReadFile(cfg.AuditLog)
	if err != nil {
		t.Fatalf("读取审计日志失败: %v", err)
	}
	auditText := string(auditData)
	t.Logf("  审计日志行数: %d", strings.Count(strings.TrimSpace(auditText), "\n")+1)
	if !strings.Contains(auditText, `"decision":"ask"`) {
		t.Error("审计日志中缺少 ask 决策记录")
	}
	if !strings.Contains(auditText, `"approved":true`) {
		t.Error("审计日志中缺少审批通过记录")
	}
}

// --------------------------------------------------------------------------- 测试辅助

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func filterType(events []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, ev := range events {
		if ev["type"] == typ {
			out = append(out, ev)
		}
	}
	return out
}

func collectText(events []map[string]any) string {
	var sb strings.Builder
	for _, ev := range events {
		if ev["type"] == "text" {
			if s, ok := ev["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

func toolNames(calls []map[string]any) []string {
	names := make([]string, 0, len(calls))
	for _, ev := range calls {
		n, _ := ev["tool_name"].(string)
		names = append(names, n)
	}
	return names
}

func resultsContain(results []map[string]any, needle string) bool {
	for _, ev := range results {
		if data, err := json.Marshal(ev["result"]); err == nil && strings.Contains(string(data), needle) {
			return true
		}
	}
	return false
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func typeHistogram(events []map[string]any) string {
	counts := map[string]int{}
	order := make([]string, 0)
	for _, ev := range events {
		typ, _ := ev["type"].(string)
		if counts[typ] == 0 {
			order = append(order, typ)
		}
		counts[typ]++
	}
	var parts []string
	for _, typ := range order {
		parts = append(parts, typ+"×"+itoa(counts[typ]))
	}
	return strings.Join(parts, " ")
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
