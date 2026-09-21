// web_proxy.go 让 web_fetch 尊重系统代理。
//
// 背景：Go 的 http.Client 默认只用环境变量代理（HTTP_PROXY / HTTPS_PROXY），
// 而用户开代理软件的「系统代理」模式通常写进 Windows 注册表（浏览器走它，
// 环境变量往往是空的）。于是浏览器能访问 GitHub、web_fetch 却超时。
//
// 方案：代理解析 = 环境变量代理（http.ProxyFromEnvironment，含 NO_PROXY）优先；
// 没有时回落到系统代理。系统代理的实际探测按平台分文件实现
// （Windows 读注册表 web_proxy_windows.go，其余平台 web_proxy_other.go）。
//
// ⚠️ 两条「不该走代理」的规则必须都生效，否则会把本机地址也塞给代理：
//  1. 系统代理的绕过列表（Windows 的 ProxyOverride）—— 用户明确列出的例外；
//  2. 回环地址（localhost / 127.* / ::1）—— 交给代理永远是错的。
package builtin

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// proxyChain 构造 http.Transport.Proxy 用的解析函数。
//
//	fixed  —— NewWebClient 时探测到的系统代理（避免每次请求都查注册表）
//	bypass —— 系统代理的绕过列表（ProxyOverride 拆出的各项）
func proxyChain(fixed *url.URL, bypass []string) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		// 环境变量代理优先（它自带 NO_PROXY 白名单语义）。
		// 但同样要尊重绕过列表与回环地址 —— 否则设了 HTTP_PROXY 时
		// 连本机地址也会被代理，行为与系统代理模式下不一致。
		if e, err := http.ProxyFromEnvironment(req); err == nil && e != nil {
			if shouldBypassProxy(req.URL.Hostname(), bypass) {
				return nil, nil
			}
			return e, nil
		}
		if fixed == nil || shouldBypassProxy(req.URL.Hostname(), bypass) {
			return nil, nil // 直连
		}
		return fixed, nil
	}
}

// systemProxy 探测当前系统的代理设置与绕过列表（非 Windows 返回 nil / nil）。
func systemProxy() (*url.URL, []string) {
	server, bypass := systemProxyRaw()
	if server == "" {
		return nil, bypass
	}
	return parseProxyServer(server), bypass
}

// shouldBypassProxy 判断某个主机是否应绕过代理。
//
// 先看回环（无条件绕过），再看系统代理的绕过列表。
// host 传 req.URL.Hostname() 即可（不含端口）。
func shouldBypassProxy(host string, bypass []string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true // 拿不到主机名就别冒险走代理
	}
	h = strings.Trim(h, "[]") // IPv6 字面量 [::1]

	// ---- 回环地址：无条件绕过 ----
	// 代理软件普遍拒绝转发到它自己所在的主机；即便能转，绕一圈也没有意义。
	if h == "localhost" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}

	// ---- 系统代理的绕过列表 ----
	for _, pat := range bypass {
		if matchProxyBypass(h, pat) {
			return true
		}
	}
	return false
}

// matchProxyBypass 按 Windows ProxyOverride 的语义匹配单条规则：
//
//	"*"             绕过所有地址
//	"<local>"       绕过不含点的主机名（如 intranet，但不含 a.example.com）
//	"*.example.com" 绕过该域及其子域
//	"192.168.*"     前缀匹配
//	"example.com"   精确匹配
func matchProxyBypass(host, pattern string) bool {
	p := strings.ToLower(strings.TrimSpace(pattern))
	if p == "" {
		return false
	}
	switch {
	case p == "*":
		return true
	case p == "<local>":
		return !strings.Contains(host, ".")
	case strings.HasPrefix(p, "*."):
		// *.example.com 同时匹配子域与裸域本身 —— 与浏览器行为一致
		return strings.HasSuffix(host, p[1:]) || host == p[2:]
	case strings.HasPrefix(p, "*"):
		return strings.HasSuffix(host, p[1:])
	case strings.HasSuffix(p, "*"):
		return strings.HasPrefix(host, strings.TrimSuffix(p, "*"))
	}
	return host == p
}

// parseProxyServer 解析 Windows ProxyServer 值的常见格式：
//
//	"127.0.0.1:7890"                        → http://127.0.0.1:7890
//	"http=127.0.0.1:7890;https=...;socks=..." → 优先 https / http / socks 段
//	"socks=127.0.0.1:7890"                  → socks5://127.0.0.1:7890
//
// 无法解析时返回 nil（保持直连）。
func parseProxyServer(s string) *url.URL {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	// 形如 "proto=host:port;..." ：按段挑 https → http → socks
	if strings.Contains(s, "=") {
		var fallback *url.URL
		for _, seg := range strings.Split(s, ";") {
			seg = strings.TrimSpace(seg)
			proto, addr, ok := strings.Cut(seg, "=")
			if !ok {
				continue
			}
			proto, addr = strings.ToLower(strings.TrimSpace(proto)), strings.TrimSpace(addr)
			u := proxyURL(proto, addr)
			if u == nil {
				continue
			}
			if proto == "https" {
				return u
			}
			if proto == "http" {
				fallback = u
				continue
			}
			if proto == "socks" && fallback == nil {
				fallback = u
			}
		}
		return fallback
	}
	// 形如 "127.0.0.1:7890"（无协议段）：按 http 代理处理
	return proxyURL("http", s)
}

// proxyURL 按协议与地址构造代理 URL；非法输入返回 nil。
func proxyURL(proto, addr string) *url.URL {
	proto = strings.ToLower(strings.TrimSpace(proto))
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == ":" {
		return nil
	}
	// 已是带 scheme 的完整 URL：原样使用
	if strings.Contains(addr, "://") {
		u, err := url.Parse(addr)
		if err != nil {
			return nil
		}
		return u
	}
	scheme := "http://"
	if proto == "socks" {
		scheme = "socks5://"
	}
	u, err := url.Parse(scheme + addr)
	if err != nil {
		return nil
	}
	return u
}
