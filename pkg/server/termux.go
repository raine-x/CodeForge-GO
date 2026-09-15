package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codeforge/pkg/platform"
)

// Termux 工具安装建议（安卓平台）：
// termux-tools 提供 termux-open-url（自动打开浏览器）与 termux-setup-storage
// （访问手机存储）等命令，缺了它们 Termux 上的「自动开浏览器」「选工作区」
// 都会降级。前端启动后查询本接口，未安装时弹建议弹窗，用户点「安装」后
// 在这里后台执行 pkg install，前端轮询状态直到装好。
//
// 状态是进程内存态（atomic），重启后按实际 LookPath 重新判定。

const (
	termuxIdle       int32 = iota // 未安装、未在装（或上次失败前）
	termuxInstalling              // 后台安装中
	termuxInstalled               // 已安装
	termuxFailed                  // 上次安装失败
)

// termuxInstallTimeout 是 pkg install 的兜底超时：Termux 换源/网络慢时
// 安装可能要几分钟，但不允许 goroutine 无限挂着。
const termuxInstallTimeout = 10 * time.Minute

// termuxToolsReady 检测 termux-tools 是否已安装。
// termux-open-url 是该包的代表性命令（自动开浏览器正依赖它），LookPath 命中即认为已装。
func termuxToolsReady() bool {
	_, err := exec.LookPath("termux-open-url")
	return err == nil
}

// handleTermuxTools 查询 / 触发安装 termux-tools。
//
// GET  → {termux, installed, installing, failed, error}
// POST → 后台执行 pkg install -y termux-tools（幂等：已装 / 已在装时直接返回当前状态）
func (s *Server) handleTermuxTools(w http.ResponseWriter, r *http.Request) {
	isTermux := platform.IsTermux()
	switch r.Method {
	case http.MethodGet:
		st := s.termuxState.Load()
		writeJSON(w, http.StatusOK, map[string]any{
			"termux":     isTermux,
			"installed":  termuxToolsReady(), // 实时检测：用户手动装了也能立刻识别
			"installing": st == termuxInstalling,
			"failed":     st == termuxFailed,
			"error":      s.termuxErr.Load(),
		})

	case http.MethodPost:
		if !isTermux {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "仅 Termux 平台支持安装 termux-tools"})
			return
		}
		if termuxToolsReady() {
			s.termuxState.Store(termuxInstalled)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "installed": true})
			return
		}
		// 已在安装中：直接返回，不重复起 goroutine（幂等）
		if !s.termuxState.CompareAndSwap(termuxIdle, termuxInstalling) &&
			!s.termuxState.CompareAndSwap(termuxFailed, termuxInstalling) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "installing": true})
			return
		}
		s.termuxErr.Store("")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "installing": true})
		go s.installTermuxTools()

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// installTermuxTools 后台执行 pkg install -y termux-tools 并落定状态。
// 成功以 LookPath 复核为准（exit 0 但命令仍缺失的异常情况按失败处理）。
func (s *Server) installTermuxTools() {
	log.Printf("[termux] 开始后台安装 termux-tools…")
	ctx, cancel := context.WithTimeout(context.Background(), termuxInstallTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pkg", "install", "-y", "termux-tools")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		err = fmt.Errorf("安装超时（%v）", termuxInstallTimeout)
	}
	if err == nil && !termuxToolsReady() {
		err = fmt.Errorf("pkg install 退出正常但未找到 termux-open-url，安装结果异常")
	}
	if err != nil {
		s.termuxErr.Store(clipText(strings.TrimSpace(string(out)), 200))
		s.termuxState.Store(termuxFailed)
		log.Printf("[termux] termux-tools 安装失败: %v", err)
		return
	}
	s.termuxErr.Store("")
	s.termuxState.Store(termuxInstalled)
	log.Printf("[termux] termux-tools 安装成功")

	// 存储尚未授权时自动跑一次 termux-setup-storage：它会弹 Android 授权框，
	// 用户同意后创建 ~/storage 软链（Termux 浏览手机存储选工作区的前提）。
	// 限时等待、失败不阻塞 —— 用户可稍后在 Termux 手动执行。
	if home, err := os.UserHomeDir(); err == nil && home != "" &&
		!dirAccessible(filepath.Join(home, "storage", "shared")) {
		if setupTermuxStorage(home) {
			log.Printf("[termux] 存储授权完成（~/storage/shared 可用）")
		} else {
			log.Printf("[termux] 存储授权未完成：如需访问手机存储，请在 Termux 执行 termux-setup-storage")
		}
	}
}
