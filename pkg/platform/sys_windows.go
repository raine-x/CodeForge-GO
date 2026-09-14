//go:build windows

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// IsTermux 在 Windows 平台恒为 false。
func IsTermux() bool { return false }

// defaultShell 返回 Windows 默认 shell 及其执行参数。
func defaultShell() (string, []string) {
	if comspec := os.Getenv("COMSPEC"); comspec != "" {
		return comspec, []string{"/c"}
	}
	return "cmd.exe", []string{"/c"}
}

// openBrowser 在 Windows 平台通过 cmd start 拉起默认浏览器。
func openBrowser(url string) error {
	return exec.Command("cmd", "/c", "start", "", url).Start()
}

// normalizePath 将正斜杠转换为 Windows 反斜杠。
func normalizePath(p string) string {
	return filepath.FromSlash(p)
}

// notify 使用 Windows Toast 通知。Windows 10/11 通常自带该 WinRT API；
// 如果系统策略禁用 Toast 或 PowerShell 不可用，返回错误但不影响任务结果。
//
// 固定 Tag/Group：同一 Tag+Group 的新通知会**替换**旧通知，
// 不会在通知中心里越堆越多（历史版本会累积多条「任务已完成」，
// 用户隔天再看到旧卡片时容易误以为「我没操作却又通知了」）。
// ExpirationTime 让通知过期后自动消失，进一步避免陈旧卡片残留。
func notify(title, message string) error {
	// PowerShell 单引号字符串转义：消息只作为文本节点写入，不拼接可执行代码。
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `$ErrorActionPreference = 'Stop'; ` +
		`[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null; ` +
		`$xml = [Windows.UI.Notifications.ToastNotificationManager]::GetTemplateContent([Windows.UI.Notifications.ToastTemplateType]::ToastText02); ` +
		`$nodes = $xml.GetElementsByTagName('text'); ` +
		`$nodes.Item(0).AppendChild($xml.CreateTextNode(` + quote(title) + `)) | Out-Null; ` +
		`$nodes.Item(1).AppendChild($xml.CreateTextNode(` + quote(message) + `)) | Out-Null; ` +
		`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml); ` +
		`$toast.Tag = 'codeforge-task'; $toast.Group = 'codeforge'; ` +
		`try { $toast.ExpirationTime = [DateTimeOffset]::Now.AddMinutes(5) } catch { } ` +
		`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('CodeForge').Show($toast)`
	return exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Run()
}
