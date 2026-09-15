// web_render.go 是 web_fetch 的 JS 渲染降级器：静态 HTTP 抓不到正文（页面依赖
// JS 动态渲染 / SPA）时，用本机无头 Chromium 渲染后再取 DOM 文本。
//
// 权衡说明：
//   - 找到浏览器才启用；Termux / 无桌面的服务器没有 Chrome 时保持纯静态抓取；
//   - 渲染有额外开销（启动浏览器秒级），只在「静态文本过短」时触发，不拖慢普通页面；
//   - SSRF 防护与静态抓取同源：进入渲染前已对初始 URL 做过私网校验（checkTarget）。
//     浏览器内部的重定向无法逐跳拦截，但初始目标已可信 —— 与系统浏览器等同的
//     信任面，本地单用户工具可接受。
package builtin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	renderTimeout   = 20 * time.Second
	renderMaxBytes  = 2 << 20
	renderMinNavBar = 120 // 静态正文 < 此长度视为可疑（JS 渲染），触发渲染降级
)

// findHeadlessBrowser 在常用位置查找可用的无头 Chromium 系浏览器路径。
// Windows 与 Linux 桌面通常装 Chrome / Edge；找不到返回空串（禁用渲染降级）。
func findHeadlessBrowser() string {
	var candidates []string
	// Windows 固定安装位置 + Edge（默认随系统存在）
	for _, p := range []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
	} {
		candidates = append(candidates, p)
	}
	// PATH 常见名称（Linux 桌面 / WSL / macOS 等）
	for _, name := range []string{
		"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "msedge",
	} {
		candidates = append(candidates, name)
	}
	for _, c := range candidates {
		if strings.TrimSpace(c) == "" {
			continue
		}
		if isExecutable(c) {
			return c
		}
	}
	return ""
}

// isExecutable 判断路径存在（PATH 名称交给 exec.LookPath，绝对路径直接 Stat）。
func isExecutable(p string) bool {
	if filepath.IsAbs(p) {
		info, err := os.Stat(p)
		return err == nil && !info.IsDir()
	}
	_, err := exec.LookPath(p)
	return err == nil
}

// renderWithBrowser 用无头浏览器渲染 URL 后 dump 整个渲染后的 DOM。
// 返回渲染后的 HTML（可能为空串），错误表示浏览器不可用/渲染失败。
//
// 关键参数 --virtual-time-budget：dump-dom 默认在页面 load 事件后立刻导出，
// 而 MSN / Next.js 这类纯前端渲染站点的正文由异步 fetch 注入（load 之后才出现），
// 立刻 dump 只能拿到脚本壳。虚拟时钟让页面把异步内容跑完再导出 —— 真实耗时
// 通常 1~4 秒（虚拟时间走得比真实快），远小于 renderTimeout 兜底。
func renderWithBrowser(ctx context.Context, exe, rawURL string) (string, error) {
	rctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--virtual-time-budget=12000", // 等待异步渲染（正文注入）完成后才 dump
		"--dump-dom",
		rawURL,
	}
	cmd := exec.CommandContext(rctx, exe, args...)
	cmd.Stderr = nil // 不吞日志进结果
	// 启动浏览器失败（如缺运行库）或 dump 超时都会以 error 返回，由调用方回退静态结果。
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	if len(out) > renderMaxBytes {
		out = out[:renderMaxBytes]
	}
	return string(out), nil
}
