package builtin

import (
	"net/http"
	"net/url"
	"testing"
)

// 取自实测机器上 ProxyOverride 的真实值，去掉与网络无关的条目。
// 用它当用例，保证「用户写的规则真的生效」而不是我自己编的规则自洽。
var realProxyOverride = []string{
	"*zhihu.com", "*zhimg.com", "*jd.com", "localhost", "*.local",
	"127.*", "10.*", "172.16.*", "172.17.*", "172.18.*", "172.19.*",
	"172.2*", "172.30.*", "172.31.*", "192.168.*",
}

func TestShouldBypassLoopbackAlways(t *testing.T) {
	// 无论绕过列表是什么（甚至为空），回环地址都必须绕过代理。
	// 代理软件普遍拒绝转发到它自己所在的主机。
	for _, host := range []string{"localhost", "127.0.0.1", "127.1.2.3", "::1"} {
		if !shouldBypassProxy(host, nil) {
			t.Errorf("%s 应无条件绕过代理", host)
		}
		if !shouldBypassProxy(host, realProxyOverride) {
			t.Errorf("%s 应绕过代理（带真实规则）", host)
		}
	}
}

// 这条是本次修复的直接目的：测试用 httptest（127.0.0.1）此前被塞给系统代理，
// 代理回 502，而 502 是「正常响应」不是连接错误，直连兜底也不会触发。
func TestShouldBypassLocalTestServer(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:54321/x")
	if !shouldBypassProxy(u.Hostname(), realProxyOverride) {
		t.Fatal("本地测试服务器（127.0.0.1）必须绕过代理，否则会被代理回 502")
	}
}

func TestShouldBypassRealRules(t *testing.T) {
	bypass := []struct{ host, why string }{
		{"www.zhihu.com", "*zhihu.com 后缀匹配"},
		{"zhihu.com", "*zhihu.com 后缀匹配（裸域）"},
		{"pic.zhimg.com", "*zhimg.com"},
		{"item.jd.com", "*jd.com"},
		{"nas.local", "*.local"},
		{"10.0.0.5", "10.*"},
		{"172.16.3.9", "172.16.*"},
		{"172.20.1.1", "172.2*"},
		{"192.168.1.1", "192.168.*"},
		{"localhost", "显式列出"},
	}
	for _, c := range bypass {
		if !shouldBypassProxy(c.host, realProxyOverride) {
			t.Errorf("%s 应绕过代理（%s）", c.host, c.why)
		}
	}

	notBypass := []string{
		"api.deepseek.com",
		"generativelanguage.googleapis.com",
		"8.8.8.8",
		"172.15.1.1", // 不在 172.16-172.31 区间内
		"172.32.1.1",
	}
	for _, host := range notBypass {
		if shouldBypassProxy(host, realProxyOverride) {
			t.Errorf("%s 不该绕过代理", host)
		}
	}
}

func TestMatchProxyBypassPatterns(t *testing.T) {
	cases := []struct {
		host, pat string
		want      bool
	}{
		{"anything.com", "*", true},
		{"intranet", "<local>", true},
		{"a.example.com", "<local>", false}, // 含点 → 不是「本地」
		{"a.example.com", "*.example.com", true},
		{"example.com", "*.example.com", true}, // 裸域也匹配
		{"badexample.com", "*.example.com", false},
		{"192.168.1.1", "192.168.*", true},
		{"192.169.1.1", "192.168.*", false},
		{"example.com", "example.com", true},
		{"sub.example.com", "example.com", false}, // 精确匹配不含子域
		{"example.com", "", false},
		{"example.com", "  ", false},
	}
	for _, c := range cases {
		if got := matchProxyBypass(c.host, c.pat); got != c.want {
			t.Errorf("matchProxyBypass(%q, %q) = %v, 期望 %v", c.host, c.pat, got, c.want)
		}
	}
}

// 大小写与首尾空白不该影响判定（注册表里的值什么形态都有）。
func TestBypassIsCaseAndSpaceInsensitive(t *testing.T) {
	if !shouldBypassProxy("  WWW.Zhihu.COM  ", []string{" *ZHIHU.com "}) {
		t.Error("匹配应忽略大小写与首尾空白")
	}
}

// IPv6 字面量带方括号时也要能识别为回环。
func TestBypassIPv6Loopback(t *testing.T) {
	if !shouldBypassProxy("[::1]", nil) {
		t.Error("[::1] 应识别为回环")
	}
}

// 拿不到主机名时保守直连，而不是冒险走代理。
func TestBypassEmptyHost(t *testing.T) {
	if !shouldBypassProxy("", nil) {
		t.Error("主机名为空时应直连")
	}
}

// proxyChain 的契约：命中绕过规则 → nil（直连）；否则 → 某个代理（非 nil）。
//
// ⚠️ 不能断言「等于我传进去的那个 fixed 代理」：环境变量代理（HTTP_PROXY）
// 优先级更高，而测试进程常常带着沙箱/CI 注入的 HTTP_PROXY。
// 这里只断言「该不该走代理」这个真正要守住的契约。
func TestProxyChainHonoursBypass(t *testing.T) {
	proxy, _ := url.Parse("http://127.0.0.1:7890")
	chain := proxyChain(proxy, []string{"*.internal"})

	// 回环地址 → 必须直连
	req, _ := url.Parse("http://127.0.0.1:8080/x")
	got, err := chain(&http.Request{URL: req})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != nil {
		t.Errorf("回环地址应直连，实际用了代理 %v", got)
	}

	// 命中绕过规则 → 必须直连
	reqInt, _ := url.Parse("http://nas.internal/x")
	gotInt, err := chain(&http.Request{URL: reqInt})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if gotInt != nil {
		t.Errorf("命中绕过规则应直连，实际用了代理 %v", gotInt)
	}

	// 普通外网地址 → 应走代理（具体是环境变量那个还是 fixed 那个不重要）
	req2, _ := url.Parse("https://api.deepseek.com/v1/models")
	got2, err := chain(&http.Request{URL: req2})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got2 == nil {
		t.Error("外网地址应走代理，实际直连了")
	}
}

// 没有系统代理、也没配环境变量代理时，应直连。
// （若进程带着 HTTP_PROXY，走代理也是对的 —— 所以这里只断言「不 panic 且不报错」。）
func TestProxyChainNilProxy(t *testing.T) {
	chain := proxyChain(nil, nil)
	req, _ := url.Parse("https://api.deepseek.com/v1/models")
	if _, err := chain(&http.Request{URL: req}); err != nil {
		t.Fatalf("不应报错: %v", err)
	}

	// 没有代理时，回环地址依然必须是直连
	reqLocal, _ := url.Parse("http://127.0.0.1:9000/x")
	got, err := chain(&http.Request{URL: reqLocal})
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != nil {
		t.Errorf("回环地址必须直连，实际 %v", got)
	}
}
