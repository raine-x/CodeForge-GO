// web_proxy.go 让 web_fetch 尊重系统代理。
//
// 背景：Go 的 http.Client 默认只用环境变量代理（HTTP_PROXY / HTTPS_PROXY），
// 而用户开代理软件的「系统代理」模式通常写进 Windows 注册表（浏览器走它，
// 环境变量往往是空的）。于是浏览器能访问 GitHub、web_fetch 却超时。
//
// 方案：代理解析 = 环境变量代理（http.ProxyFromEnvironment，含 NO_PROXY）优先；
// 没有时回落到系统代理。系统代理的实际探测按平台分文件实现
// （Windows 读注册表 web_proxy_windows.go，其余平台 web_proxy_other.go）。
package builtin

import (
	"net/http"
	"net/url"
	"strings"
)

// proxyChain 构造 http.Transport.Proxy 用的解析函数。
// fixed 为 NewWebClient 时探测到的系统代理（避免每次请求都查注册表）。
func proxyChain(fixed *url.URL) func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if e, err := http.ProxyFromEnvironment(req); err == nil && e != nil {
			return e, nil // 环境变量代理优先（含 NO_PROXY 白名单语义）
		}
		return fixed, nil // 系统代理兜底
	}
}

// systemProxy 探测当前系统的代理设置（非 Windows 返回 nil）。
func systemProxy() *url.URL {
	server := systemProxyServer()
	if server == "" {
		return nil
	}
	return parseProxyServer(server)
}

// systemProxyServer 返回当前系统配置的代理地址（未启用或不存在返回空串）。
// 平台相关实现：Windows 读注册表（web_proxy_windows.go），其余平台统返回空
// （web_proxy_other.go）。

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
