// web.go 提供联网工具：web_fetch（读取网页正文）与 web_search（联网搜索）。
//
// SSRF 防护（防「模型诱导程序访问内网」）：
//   - 只允许 http/https 协议；
//   - 请求前先解析 DNS，拒绝回环/私网/link-local/多播地址（除非 allow_private:true）；
//   - 重定向的每个目标都重新执行同一套校验（防「先放行再跳内网」）。
//
// 实现保持无状态：web_fetch 直连目标，web_search 通过 WebSearcher 接口
// 支持 Tavily（POST JSON）与 SearxNG（?format=json）两种后端。
package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"codeforge/config"
	"codeforge/pkg/tools"
)

const (
	webFetchTimeout  = 15 * time.Second
	webFetchMaxBytes = 2 << 20 // 读取上限 2MB，防超大页面耗尽上下文
)

// WebSearcher 抽象联网搜索后端（Tavily / SearxNG）。
type WebSearcher interface {
	Search(ctx context.Context, q string, n int) ([]WebHit, error)
}

// WebHit 是一条搜索结果。
type WebHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// WebClient 是 web_fetch / web_search 的共享客户端（含 SSRF 校验）。
type WebClient struct {
	http         *http.Client // 走代理链（环境变量代理优先，系统代理兜底）
	direct       *http.Client // 无代理直连：代理不可用时回落（见 FetchTool.Execute）
	allowPrivate bool
	searcher     WebSearcher // 可为 nil：仅 web_fetch 时
}

// NewWebClient 按配置构造联网客户端。searcher 由 RegisterWeb 决定（provider 已知才注入）。
func NewWebClient(cfg config.WebConfig, searcher WebSearcher) *WebClient {
	client := &WebClient{allowPrivate: cfg.AllowPrivate, searcher: searcher}
	client.http = &http.Client{
		Timeout: webFetchTimeout,
		// 代理链：环境变量代理（HTTP_PROXY 等）优先，否则回落系统代理 ——
		// 用户开着代理软件但没设环境变量时，直连 GitHub 会超时（见 web_proxy.go）。
		Transport:     &http.Transport{Proxy: proxyChain(systemProxy())},
		CheckRedirect: redirectCheck(client),
	}
	// 无代理直连客户端：实测有些代理软件（TUN/透明转发）对 Go 的 CONNECT 隧道返回 EOF，
	// 但直连反而不走代理就能通。代理路径失败时用它兜底，绝不能让单纯网络问题拖死抓取。
	client.direct = &http.Client{
		Timeout:       webFetchTimeout,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: redirectCheck(client),
	}
	return client
}

// redirectCheck 每次重定向都重新校验目标：SSRF 常见绕法 = 先给公网 URL 再 302 到内网。
func redirectCheck(client *WebClient) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("重定向次数过多")
		}
		return client.checkTarget(req.Context(), req.URL)
	}
}

// checkTarget 校验 URL 是否允许访问：仅 http(s)、非私网。
func (c *WebClient) checkTarget(ctx context.Context, u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅支持 http/https，拒绝协议 %q", u.Scheme)
	}
	if c.allowPrivate {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("拒绝访问内网/回环地址 %s（web.allow_private=true 可放行）", host)
		}
		return nil
	}
	// 域名：解析全部 A 记录，任一命中私网即拒绝。
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("解析 %s 失败: %v", host, err)
	}
	for _, ia := range ips {
		if isPrivateIP(ia.IP) {
			return fmt.Errorf("拒绝访问解析到内网/回环地址 %s（%s）", host, ia.IP)
		}
	}
	return nil
}

// isPrivateIP 判断 IP 是否属于回环/私网/link-local/多播/保留段（SSRF 黑名单）。
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	return false
}

// htmlToText 把 HTML 粗略转成纯文本：去标签、解码实体、折叠空行，保留标题与链接文本。
func htmlToText(html []byte) string {
	// 简易实现：按 tag 切，保留可见文本。
	s := string(html)
	var b strings.Builder
	for len(s) > 0 {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break
		}
		s = s[i+end+1:]
	}
	text := b.String()
	// 折叠连续空白为单空格
	text = strings.Join(strings.Fields(text), " ")
	// 实体解码最常用的几个
	repl := strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`)
	text = repl.Replace(text)
	// 截断过长文本（单次读取上限已挡超大页面，这里再按字符保护上下文）
	if r := []rune(text); len(r) > 4000 {
		text = string(r[:4000]) + "…"
	}
	return text
}

// FetchTool 是 web_fetch 工具。
// render 为 JS 渲染降级器：静态抓取拿不到正文（SPA / 动态渲染）时调用，
// 用本机无头浏览器渲染后取 DOM 文本。nil 表示无渲染器（Termux 等），保持纯静态。
type FetchTool struct {
	c      *WebClient
	render func(ctx context.Context, rawURL string) (string, error)
}

// Name 实现 tools.Tool。
func (t *FetchTool) Name() string { return "web_fetch" }

// Description 实现 tools.Tool。
func (t *FetchTool) Description() string {
	return "读取一个网页并返回纯文本正文（自动去标签、解实体、超长截断）。" +
		"何时调用：确知具体 URL 且需要其**正文内容**时 —— 引用原文、查库/框架的版本说明、读文档/帮助页、验证线上页面。" +
		"何时不调用：不知道确切 URL 时先搜索网页找链接，不要猜 URL；不要用它当搜索引擎（它只读单页）。" +
		"每次只抓取一个 URL。只接受公网 http(s) 地址（内网/回环地址不可用，属预期，勿重试）。" +
		"页面正文由静态 HTML 或渲染后内容提供，两种来源都是工具自动完成，无需额外操作；需要登录才可见的页面拿不到属预期，不要再改用系统命令或脚本抓同一个地址，如实告诉用户即可。"
}

// InputSchema 实现 tools.Tool。
func (t *FetchTool) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("url", "要读取的公网 http(s) 网址（一次一个，完整 URL 含协议，如 https://example.com/docs）", true).
		Int("max_bytes", "最大读取字节数，默认 2MB；结果还会按字符截断到约 4000 字", false).
		Build()
}

// IsReadOnly 声明只读（网络读取）。
func (t *FetchTool) IsReadOnly() bool { return true }

// Execute 实现 tools.Tool。
func (t *FetchTool) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	var p struct {
		URL      string `json:"url"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if strings.TrimSpace(p.URL) == "" {
		return tools.Err("url 不能为空"), nil
	}
	u, err := url.Parse(p.URL)
	if err != nil {
		return tools.Err("URL 无效: %v", err), nil
	}
	if err := t.c.checkTarget(ctx, u); err != nil {
		return tools.Err("%v", err), nil
	}
	maxBytes := p.MaxBytes
	if maxBytes <= 0 || maxBytes > webFetchMaxBytes {
		maxBytes = webFetchMaxBytes
	}

	// 请求序列：
	//   1) 代理链（环境变量代理优先 → 系统代理），失败立刻重试一次 ——
	//      实测代理软件对 CONNECT 隧道有偶发 EOF（第 1 次握手失败、第 2 次即通），
	//      重试成本远低于直连超时（对会墙的站点直连要等满 timeout）。
	//   2) 仍失败则回落无代理直连兜底（TUN/透明转发场景直连可能比代理更稳）。
	//   3) 都失败再交给无头浏览器渲染补救（浏览器自走系统代理，协议字段更完整）。
	usedProxy := true
	var out *http.Response
	for attempt := 0; attempt < 2 && out == nil; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return tools.Err("构造请求失败: %v", err), nil
		}
		req.Header.Set("User-Agent", "CodeForge/1.0 (+web_fetch)")
		if resp, err := t.c.http.Do(req); err == nil {
			out = resp
		}
	}
	var lastErr error
	if out == nil {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return tools.Err("构造请求失败: %v", err), nil
		}
		req.Header.Set("User-Agent", "CodeForge/1.0 (+web_fetch)")
		out, lastErr = t.c.direct.Do(req)
		usedProxy = false
	}
	if out == nil {
		// 静态直连/代理均失败。让走系统代理的无头浏览器补救一次；失败才按网络错误上报，不重复打命令。
		if t.render != nil {
			if dom, rerr := t.render(ctx, u.String()); rerr == nil {
				if rtext := htmlToText([]byte(dom)); len([]rune(strings.TrimSpace(rtext))) > 0 {
					return tools.Ok(map[string]any{
						"final_url": u.String(),
						"content":   rtext,
						"rendered":  true,
						"note":      "静态直连失败，结果来自无头浏览器渲染",
					}), nil
				}
			}
		}
		return tools.Err("请求失败: %v", lastErr), nil
	}
	resp := out
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return tools.Err("目标返回 %s", resp.Status), nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)))
	if err != nil {
		return tools.Err("读取响应失败: %v", err), nil
	}
	text := htmlToText(body)

	// JS 渲染降级：静态正文过短（疑似 SPA/动态渲染）且有渲染器时，用无头浏览器再取一次。
	// 渲染成功且内容更长才替换；失败/无渲染器都回退静态结果，不升级为错误。
	rendered := false
	if t.render != nil && len([]rune(strings.TrimSpace(text))) < renderMinNavBar {
		if dom, rerr := t.render(ctx, u.String()); rerr == nil && len([]rune(strings.TrimSpace(htmlToText([]byte(dom))))) > renderMinNavBar {
			text = htmlToText([]byte(dom))
			rendered = true
		}
	}
	if strings.TrimSpace(text) == "" {
		return tools.Err("目标内容为空或不可解析（可能不是 HTML，或页面需登录）"), nil
	}
	return tools.Ok(map[string]any{
		"final_url": resp.Request.URL.String(),
		"content":   text,
		"rendered":  rendered,
		"direct":    !usedProxy, // 最终经过无代理直连取得（代理路径曾失败）
		"truncated": len(body) >= maxBytes,
	}), nil
}

// SearchToolWeb 是 web_search 工具。
type SearchToolWeb struct{ c *WebClient }

// Name 实现 tools.Tool。
func (t *SearchToolWeb) Name() string { return "web_search" }

// Description 实现 tools.Tool。
func (t *SearchToolWeb) Description() string {
	return "联网搜索并返回结果列表（标题 / URL / 摘要，默认 5 条）。" +
		"何时调用：需要**当前/最新**信息（库的新版本、已知问题、实时资讯）、项目之外的知识、或本地检索不到的内容时 —— 先于 fetch 使用，由它给出 URL。" +
		"何时不调用：问题只涉及工作区内的代码/文件（用搜索文件/读取文件，别联网）；内容依赖具体页面正文时（拿到 URL 后再用读取网页）。" +
		"对搜索到的链接，必要时再用读取网页打开具体页面核实内容。引用网络信息时附上来源 URL。"
}

// InputSchema 实现 tools.Tool。
func (t *SearchToolWeb) InputSchema() json.RawMessage {
	return tools.NewSchema().
		Str("query", "搜索关键词：简洁、聚焦主题（如「anthropic claude api rate limits」）", true).
		Int("count", "返回条数，默认 5，最大 10（更多条可以，但会占用上下文）", false).
		Build()
}

// IsReadOnly 声明只读。
func (t *SearchToolWeb) IsReadOnly() bool { return true }

// Execute 实现 tools.Tool。
func (t *SearchToolWeb) Execute(ctx context.Context, args json.RawMessage) (*tools.ToolResult, error) {
	if t.c.searcher == nil {
		return tools.Err("未配置搜索后端（config: web.search_provider）"), nil
	}
	var p struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return tools.Err("参数解析失败: %v", err), nil
	}
	if strings.TrimSpace(p.Query) == "" {
		return tools.Err("query 不能为空"), nil
	}
	n := p.Count
	if n <= 0 {
		n = 5
	}
	if n > 10 {
		n = 10
	}
	hits, err := t.c.searcher.Search(ctx, p.Query, n)
	if err != nil {
		return tools.Err("搜索失败: %v", err), nil
	}
	if len(hits) == 0 {
		return tools.Ok([]WebHit{}), nil
	}
	return tools.Ok(hits), nil
}

// tavilySearcher 通过 Tavily API 搜索（POST JSON）。
type tavilySearcher struct {
	apiKey string
}

func (s *tavilySearcher) Search(ctx context.Context, q string, n int) ([]WebHit, error) {
	payload, _ := json.Marshal(map[string]any{
		"api_key":        s.apiKey,
		"query":          q,
		"max_results":    n,
		"include_answer": false,
		"search_depth":   "basic",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.tavily.com/search", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("Tavily 返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var d struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("解析 Tavily 响应失败: %v", err)
	}
	out := make([]WebHit, 0, len(d.Results))
	for _, r := range d.Results {
		out = append(out, WebHit{Title: r.Title, URL: r.URL, Snippet: strings.TrimSpace(r.Content)})
	}
	return out, nil
}

// searxngSearcher 通过 SearxNG 实例的 JSON 端点搜索。
type searxngSearcher struct {
	endpoint string
}

func (s *searxngSearcher) Search(ctx context.Context, q string, n int) ([]WebHit, error) {
	u, _ := url.Parse(s.endpoint)
	// 兼容调用方只填 /search 的情况：确保带 query 参数与 json 格式
	qp := u.Query()
	qp.Set("q", q)
	qp.Set("format", "json")
	u.RawQuery = qp.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("SearxNG 返回 %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var d struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("解析 SearxNG 响应失败: %v", err)
	}
	out := make([]WebHit, 0, len(d.Results))
	for i, r := range d.Results {
		if i >= n {
			break
		}
		out = append(out, WebHit{Title: r.Title, URL: r.URL, Snippet: strings.TrimSpace(r.Content)})
	}
	return out, nil
}

// NewSearcher 按配置构造搜索后端；未配置（provider 未知/无 key）时返回 nil。
func NewSearcher(cfg config.WebConfig) WebSearcher {
	switch strings.ToLower(strings.TrimSpace(cfg.SearchProvider)) {
	case "tavily":
		key := strings.TrimSpace(cfg.APIKeyEnv)
		if key == "" {
			key = "TAVILY_API_KEY"
		}
		apiKey := getenv(key)
		if apiKey == "" {
			return nil
		}
		return &tavilySearcher{apiKey: apiKey}
	case "searxng":
		if strings.TrimSpace(cfg.SearchEndpoint) == "" {
			return nil
		}
		return &searxngSearcher{endpoint: cfg.SearchEndpoint}
	default:
		return nil
	}
}

// getenv 读取环境变量（空串视为未配置）。
func getenv(k string) string { return os.Getenv(k) }

// RegisterWeb 注册联网工具（web_fetch 始终注册；web_search 仅在搜索后端可用时注册）。
// enabled=false 时两者都不注册（完全关闭联网能力）。
// web_fetch 顺带注入 JS 渲染降级器：本机有无头浏览器则静态抓取拿不到正文时会自动渲染。
func RegisterWeb(reg *tools.Registry, cfg config.WebConfig) {
	if !cfg.WebEnabledAt() {
		return
	}
	client := NewWebClient(cfg, NewSearcher(cfg))
	fetch := &FetchTool{c: client}
	if exe := findHeadlessBrowser(); exe != "" {
		fetch.render = func(ctx context.Context, rawURL string) (string, error) {
			return renderWithBrowser(ctx, exe, rawURL)
		}
	}
	reg.Register(fetch)
	if client.searcher != nil {
		reg.Register(&SearchToolWeb{c: client})
	}
}
