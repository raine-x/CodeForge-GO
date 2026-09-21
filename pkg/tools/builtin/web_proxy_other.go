//go:build !windows

package builtin

// systemProxyRaw 非 Windows 平台：系统代理概念不统一（mac/Linux 无注册表），
// 依赖环境变量代理即可（proxyChain 已优先处理 ProxyFromEnvironment），返回空。
func systemProxyRaw() (string, []string) { return "", nil }
