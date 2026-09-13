package platform

import "errors"

// ErrNoBrowser 表示未找到可用的浏览器拉起方式。
var ErrNoBrowser = errors.New("未找到可用的浏览器打开方式，请手动访问服务地址")

// Shell 返回当前平台的默认 shell 及其执行参数。
func Shell() (string, []string) { return defaultShell() }

// OpenBrowser 使用系统默认方式打开浏览器。
func OpenBrowser(url string) error { return openBrowser(url) }

// Notify 发送系统级任务通知。平台不支持或未安装通知命令时返回错误，调用方可忽略。
func Notify(title, message string) error { return notify(title, message) }

// NormalizePath 按当前平台规范化路径分隔符。
func NormalizePath(p string) string { return normalizePath(p) }
