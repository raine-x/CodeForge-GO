//go:build !windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// IsTermux 判断当前是否运行在 Android Termux 环境。
func IsTermux() bool {
	if runtime.GOOS == "android" {
		return true
	}
	return strings.Contains(os.Getenv("PREFIX"), "com.termux")
}

// defaultShell 返回 POSIX 平台默认 shell 及其执行参数。
func defaultShell() (string, []string) {
	if IsTermux() {
		if prefix := os.Getenv("PREFIX"); prefix != "" {
			sh := filepath.Join(prefix, "bin", "bash")
			if _, err := os.Stat(sh); err == nil {
				return sh, []string{"-c"}
			}
		}
	}
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh, []string{"-c"}
		}
	}
	return "/bin/sh", []string{"-c"}
}

// openBrowser 在 POSIX / Termux 平台拉起浏览器。
func openBrowser(url string) error {
	if IsTermux() {
		if _, err := exec.LookPath("termux-open-url"); err == nil {
			return exec.Command("termux-open-url", url).Start()
		}
	}
	if _, err := exec.LookPath("xdg-open"); err == nil {
		return exec.Command("xdg-open", url).Start()
	}
	if _, err := exec.LookPath("open"); err == nil { // macOS 兜底
		return exec.Command("open", url).Start()
	}
	return ErrNoBrowser
}

// notify 发送 Linux 桌面通知；Termux 优先调用 termux-notification，
// 普通 Linux 使用 notify-send，二者都没有时返回错误（通知失败不影响任务）。
func notify(title, message string) error {
	if IsTermux() {
		if _, err := exec.LookPath("termux-notification"); err != nil {
			return err
		}
		return exec.Command("termux-notification", "--title", title, "--content", message,
			"--id", "codeforge-task").Run()
	}
	if _, err := exec.LookPath("notify-send"); err == nil {
		return exec.Command("notify-send", title, message).Run()
	}
	if _, err := exec.LookPath("zenity"); err == nil {
		return exec.Command("zenity", "--notification", "--text", title+": "+message).Run()
	}
	return errors.New("未找到 notify-send/zenity")
}

// normalizePath 在 POSIX 平台不做转换。
func normalizePath(p string) string { return p }
