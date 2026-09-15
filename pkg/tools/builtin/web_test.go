package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"codeforge/config"
	"codeforge/pkg/tools"
)

// 联网工具（web_fetch / web_search）的回归测试。
//
// 重点锁 SSRF 防护（模型诱导程序访问内网是首要风险面）：
//   - 回环/私网地址必须拒绝，除非 allow_private=true；
//   - 重定向目标要重新校验（302 到内网是常见绕法）；
//   - 正常公网抓取可用、HTML 转纯文本、超限被截断。
// 全部用 httptest 起本地服务（主机名 example.com 解析到 127.0.0.1 也算回环，
// 但直接给 IP 更明确，故用 127.0.0.1 作为「私网目标」代表）。

func webCfg(enabled bool) config.WebConfig {
	e := enabled
	return config.WebConfig{Enabled: &e}
}

// runFetch 同步执行一次 web_fetch（返回工具结果）。
func runFetch(t *testing.T, tool *FetchTool, args string) *tools.ToolResult {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	return res
}

// dataText 取成功结果的 content（web_fetch 的 map 字段）。
func dataText(res *tools.ToolResult) string {
	if res == nil || !res.Success {
		return ""
	}
	if m, ok := res.Data.(map[string]any); ok {
		if c, ok := m["content"].(string); ok {
			return c
		}
	}
	return ""
}

// TestWebFetchNormalPublic 正常抓取：HTML 转纯文本、带最终 URL。
// 本地起 httptest（127.0.0.1 属回环），因此这里用 allow_private=true 的 client；
// 私网默认拒绝由 TestWebFetchRejectsLoopback 单独覆盖。
func TestWebFetchNormalPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><head><title>示例</title></head><body><h1>你好</h1><p>世界 &amp; 你好</p></body></html>`)
	}))
	defer srv.Close()

	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	res := runFetch(t, &FetchTool{c: c}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("正常抓取应成功: %+v", res)
	}
	text := dataText(res)
	if !strings.Contains(text, "你好") || !strings.Contains(text, "世界 & 你好") {
		t.Errorf("HTML 应转为纯文本且实体解码: %q", text)
	}
	if strings.Contains(text, "<") {
		t.Errorf("纯文本不应残留标签: %q", text)
	}
}

// TestWebFetchRejectsLoopback web_fetch 必须拒绝回环地址（SSRF 防护，allow_private=false）。
func TestWebFetchRejectsLoopback(t *testing.T) {
	c := NewWebClient(webCfg(true), nil)
	res := runFetch(t, &FetchTool{c: c}, fmt.Sprintf(`{"url":"http://127.0.0.1:%d/admin"}`, 8080))
	if res.Success {
		t.Fatalf("访问回环地址必须被拒: %+v", res)
	}
	if !strings.Contains(res.Error, "拒绝") {
		t.Errorf("拒绝文案应说明原因: %q", res.Error)
	}
}

// TestWebFetchAllowPrivate 显式 allow_private=true 时允许内网访问。
func TestWebFetchAllowPrivate(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	res := runFetch(t, &FetchTool{c: c}, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("allow_private=true 应放行内网: %+v", res)
	}
	if !hit {
		t.Error("服务端应收到请求")
	}
}

// TestWebFetchRewriteTarget 重定向目标必须重新校验（CheckRedirect 复查）。
// 直接构造私网目标 URL，验证 checkTarget 对这一目标的拒绝 —— 初始 URL 与
// 重定向目标共用同一道防线，302→内网的绕法在这里被拦下。
func TestWebFetchRewriteTarget(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target := fmt.Sprintf("http://127.0.0.1:%d/secret", ln.Addr().(*net.TCPAddr).Port)

	c := NewWebClient(webCfg(true), nil)
	if err := c.checkTarget(context.Background(), mustParse(t, target)); err == nil {
		t.Error("checkTarget 应拒绝私网重定向目标")
	}
}

// TestWebFetchLimit 超过 max_bytes 时截断，不把超大页面全塞进上下文。
func TestWebFetchLimit(t *testing.T) {
	big := strings.Repeat("a", 5<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, big)
	}))
	defer srv.Close()

	// 小上限（256 字符）
	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	res := runFetch(t, &FetchTool{c: c}, fmt.Sprintf(`{"url":%q,"max_bytes":256}`, srv.URL))
	if !res.Success {
		t.Fatalf("截断读取不该算失败: %+v", res)
	}
	if len(dataText(res)) > 4001 { // 截断到 4000 字符 + 省略号
		t.Errorf("内容应被截断控制长度，实际 %d 字符", len(dataText(res)))
	}
}

// 工具描述必须「规定何时调用」而不只是功能说明（官方 tool-use 最佳实践：触发条件
// 显著提升模型的应该调用率）。防未来重写成「抓取网页」「联网搜索」这类弱描述。
func TestWebToolDescriptionsPrescribeWhenToCall(t *testing.T) {
	for _, d := range []string{(NewWebToolFetch()).Description(), (NewWebToolSearch()).Description()} {
		if !strings.Contains(d, "何时调用") {
			t.Errorf("工具描述应包含「何时调用」触发条件：%q", d)
		}
		if !strings.Contains(d, "何时不调用") {
			t.Errorf("工具描述应包含「何时不调用」边界：%q", d)
		}
	}
}

func NewWebToolFetch() *FetchTool      { return &FetchTool{c: NewWebClient(webCfg(true), nil)} }
func NewWebToolSearch() *SearchToolWeb { return &SearchToolWeb{c: NewWebClient(webCfg(true), nil)} }

// dataField 取成功结果 Data map 里的指定字段（web_fetch 把 rendered/truncated 放这里）。
func dataField(res *tools.ToolResult, key string) (any, bool) {
	if res == nil || !res.Success {
		return nil, false
	}
	m, ok := res.Data.(map[string]any)
	if !ok {
		return nil, false
	}
	v, ok := m[key]
	return v, ok
}

// JS 渲染降级：静态正文过短（SPA 壳）时自动用渲染器重取；正常页面不触发。
func TestWebFetchFallsBackToRenderer(t *testing.T) {
	// 服务端返回一个「壳」：HTML 只有脚本与占位，静态转文本为空/极短。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<html><body><script>document.write('「渲染出来的正文：' + '100字内容……' )</script><div id="root"></div></body></html>`)
	}))
	defer srv.Close()

	var renderedURL string
	fakeRender := func(_ context.Context, rawURL string) (string, error) {
		renderedURL = rawURL
		// 渲染后的 DOM：正文出现了
		return `<html><body><article>` + strings.Repeat("渲染正文", 40) + `</article></body></html>`, nil
	}

	// 无渲染器：静态壳 → 返回脚本文本（非空即按正常结果返回），rendered=false
	plain := &FetchTool{c: NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)}
	res := runFetch(t, plain, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("静态壳非空应返回成功: %+v", res)
	}
	if v, _ := dataField(res, "rendered"); v == true {
		t.Error("无渲染器时 rendered 必须为 false")
	}
	if strings.Contains(dataText(res), "渲染正文") {
		t.Errorf("无渲染器时不应有渲染后内容: %q", dataText(res))
	}

	// 有渲染器：自动触发，正文来自渲染结果且 rendered=true
	withRenderer := &FetchTool{
		c:      NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil),
		render: fakeRender,
	}
	res2 := runFetch(t, withRenderer, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res2.Success {
		t.Fatalf("渲染降级应成功: %+v", res2)
	}
	if v, _ := dataField(res2, "rendered"); v != true {
		t.Error("降级结果应标记 rendered=true")
	}
	if !strings.Contains(dataText(res2), "渲染正文") {
		t.Errorf("内容应来自渲染后 DOM: %q", dataText(res2))
	}
	if renderedURL == "" {
		t.Error("应把 URL 传给渲染器")
	}
}

// 正常页面（正文足够）不触发渲染：renderer 不应被调用。
func TestWebFetchDoesNotRenderNormalPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("正常正文", 60)+"</p></body></html>")
	}))
	defer srv.Close()

	called := false
	tool := &FetchTool{
		c: NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil),
		render: func(context.Context, string) (string, error) {
			called = true
			return "", nil
		},
	}
	res := runFetch(t, tool, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("正常页面应成功: %+v", res)
	}
	if called {
		t.Error("正文足够时不应触发渲染降级")
	}
	if !strings.Contains(dataText(res), "正常正文") {
		t.Errorf("应返回静态正文: %q", dataText(res))
	}
}

// 无渲染器时返回结果不带 rendered 标记（缺省 false 即可）。
func TestWebFetchRendererMetadataAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("x", 500)+"</p></body></html>")
	}))
	defer srv.Close()
	tool := &FetchTool{c: NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)}
	res := runFetch(t, tool, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("应成功: %+v", res)
	}
	if v, ok := dataField(res, "rendered"); ok && v.(bool) {
		t.Error("未渲染时 rendered 应为 false")
	}
}

// TestWebSearchRequiresBackend 未配置搜索后端时 web_search 报明确错误而不是静默。
func TestWebSearchRequiresBackend(t *testing.T) {
	c := NewWebClient(webCfg(true), nil) // searcher=nil
	tool := &SearchToolWeb{c: c}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"query":"hello"}`))
	if res.Success {
		t.Fatalf("无后端时应报错: %+v", res)
	}
	if !strings.Contains(res.Error, "未配置搜索后端") {
		t.Errorf("报错应说明原因: %q", res.Error)
	}
}

// TestWebSearchSearxng 通过 httptest 模拟 SearxNG JSON 端点，验证解析。
func TestWebSearchSearxng(t *testing.T) {
	var gotQ string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query().Get("q")
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("应请求 json 格式，实际 %s", r.URL.Query().Get("format"))
		}
		fmt.Fprint(w, `{"results":[
			{"title":"标题一","url":"https://a.example/x","content":"摘要一"},
			{"title":"标题二","url":"https://b.example/y","content":"摘要二"}
		]}`)
	}))
	defer srv.Close()

	c := NewWebClient(config.WebConfig{
		Enabled:        boolPtr(true),
		SearchProvider: "searxng",
		SearchEndpoint: srv.URL,
	}, NewSearcher(config.WebConfig{SearchProvider: "searxng", SearchEndpoint: srv.URL}))

	tool := &SearchToolWeb{c: c}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"query":"codeforge","count":5}`))
	if !res.Success {
		t.Fatalf("搜索应成功: %+v", res)
	}
	if gotQ != "codeforge" {
		t.Errorf("查询词应透传: %q", gotQ)
	}
	hits, ok := res.Data.([]WebHit)
	if !ok || len(hits) != 2 {
		t.Fatalf("应解析出 2 条结果: %+v", res.Data)
	}
	if hits[0].Title != "标题一" || hits[0].URL != "https://a.example/x" {
		t.Errorf("结果字段解析错误: %+v", hits[0])
	}
}

// ---- 系统代理 ----

// parseProxyServer 覆盖 Windows ProxyServer 注册表值的常见格式。
func TestParseProxyServer(t *testing.T) {
	cases := []struct {
		in   string
		want string // 期望的完整代理 URL；空 = 应为 nil
	}{
		{"127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"http://127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"http=127.0.0.1:7890;https=127.0.0.1:7890", "http://127.0.0.1:7890"},
		{"socks=127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"", ""},
		{"   ", ""},
		{"=", ""},
	}
	for _, c := range cases {
		got := parseProxyServer(c.in)
		if c.want == "" {
			if got != nil {
				t.Errorf("parseProxyServer(%q) 应为 nil，实际 %v", c.in, got)
			}
			continue
		}
		if got == nil || got.String() != c.want {
			t.Errorf("parseProxyServer(%q) = %v，期望 %q", c.in, got, c.want)
		}
	}
}

// 静态直连失败（如只开系统代理未设环境变量，直连 GitHub 超时）时，
// 有渲染器必须让浏览器补救成功，而不是直接返回网络错误。
// 注：禁用代理强制走直连（本机系统代理会把 127.0.0.1:1 转发并返回 502，非本测试场景）。
func TestWebFetchRendererRescuesNetworkFailure(t *testing.T) {
	// 无人监听的本地端口 → 连接立即失败（allow_private=true 放行回环）。
	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	disableProxy(c)
	tool := &FetchTool{
		c: c,
		render: func(_ context.Context, _ string) (string, error) {
			return `<html><body><article>` + strings.Repeat("浏览器渲染的正文", 40) + `</article></body></html>`, nil
		},
	}
	res := runFetch(t, tool, `{"url":"http://127.0.0.1:1/x"}`)
	if !res.Success {
		t.Fatalf("静态失败应被渲染补救: %+v", res)
	}
	if v, _ := dataField(res, "rendered"); v != true {
		t.Error("补救结果应标记 rendered=true")
	}
	if !strings.Contains(dataText(res), "浏览器渲染的正文") {
		t.Errorf("内容应来自渲染: %q", dataText(res))
	}
}

// 静态失败且无渲染器：如实报网络错误（不假装成功）。
func TestWebFetchNetworkFailureWithoutRenderer(t *testing.T) {
	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	disableProxy(c)
	tool := &FetchTool{c: c}
	res := runFetch(t, tool, `{"url":"http://127.0.0.1:1/x"}`)
	if res.Success {
		t.Fatalf("无渲染器时静态失败应报错: %+v", res)
	}
	if !strings.Contains(res.Error, "请求失败") {
		t.Errorf("应报告网络错误: %q", res.Error)
	}
}

// 代理路径失效（本机实际环境：系统代理 127.0.0.1:7890 对 Go 返回 EOF）时，
// 必须自动回落无代理直连并成功 —— 而不仅是报网络错误或等渲染器。
func TestWebFetchFallsBackToDirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("直连正文", 60)+"</p></body></html>")
	}))
	defer srv.Close()

	// 构造 WebClient 后强制把主客户端打入死代理（端口 1 无人监听，连接立刻失败），
	// direct 客户端保持无代理 —— 模拟「注册表代理坏、直连反而通」的真实场景。
	c := NewWebClient(config.WebConfig{Enabled: boolPtr(true), AllowPrivate: true}, nil)
	deadProxy, _ := url.Parse("http://127.0.0.1:1")
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(deadProxy)
	}

	tool := &FetchTool{c: c}
	res := runFetch(t, tool, fmt.Sprintf(`{"url":%q}`, srv.URL))
	if !res.Success {
		t.Fatalf("代理坏时应回落直连成功: %+v", res)
	}
	if !strings.Contains(dataText(res), "直连正文") {
		t.Errorf("内容应来自直连: %q", dataText(res))
	}
	if v, ok := dataField(res, "direct"); !ok || v != true {
		t.Error("回落结果应标记 direct=true")
	}
}

// disableProxy 禁用 WebClient 主客户端的代理，强制走直连。
// 测试环境本机系统代理会把 127.0.0.1:1 等回环请求转发并返回 502，
// 与「直连失败」类测试的意图不符，故显式关掉。
func disableProxy(c *WebClient) {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.Proxy = nil
	}
	if tr, ok := c.direct.Transport.(*http.Transport); ok {
		tr.Proxy = nil
	}
}

func boolPtr(b bool) *bool { return &b }

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
