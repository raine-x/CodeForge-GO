//go:build windows

package builtin

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// systemProxyRaw 读取 Windows 系统代理（注册表 Internet Settings）。
//
// 返回代理地址**与绕过列表**。代理未启用或读取失败时 server 为空串。
//
// ⚠️ 必须把 ProxyOverride 一起读出来。
//
//	此前只读了 ProxyServer，把「哪些地址不该走代理」这条信息整个丢掉了 ——
//	用户/代理软件在 ProxyOverride 里明确写着 `localhost;127.*;10.*;*.local`，
//	却依然被塞给代理，于是连本机地址都走代理、代理回 502。
//	2026-09-21 实测：6 个 WebFetch 测试因此全部失败（测试目标是本地 httptest）。
func systemProxyRaw() (string, []string) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return "", nil
	}
	defer k.Close()

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enable == 0 {
		return "", nil
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return "", nil
	}

	// ProxyOverride 缺失是正常情况（表示没有额外绕过规则），不当错误处理。
	var bypass []string
	if ov, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		for _, seg := range strings.Split(ov, ";") {
			if seg = strings.TrimSpace(seg); seg != "" {
				bypass = append(bypass, seg)
			}
		}
	}
	return server, bypass
}
