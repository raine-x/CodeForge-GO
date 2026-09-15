//go:build windows

package builtin

import "golang.org/x/sys/windows/registry"

// systemProxyServer 读取 Windows 系统代理（注册表 Internet Settings）。
// 代理未启用或读取失败返回空串。
func systemProxyServer() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return ""
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return server
}