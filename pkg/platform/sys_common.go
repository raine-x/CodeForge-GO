// Package platform 提供跨平台抽象层，隔离 Linux / Termux / Windows 的差异。
package platform

import "runtime"

// OSName 返回可读的平台名称：windows / linux / termux。
func OSName() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "android":
		return "termux"
	case "linux":
		if IsTermux() {
			return "termux"
		}
		return "linux"
	default:
		return runtime.GOOS
	}
}
