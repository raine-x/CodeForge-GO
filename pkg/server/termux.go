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
//
// ⚠️ 不能只看 PATH 里的 termux-open-url 一个命令：
//   - CodeForge 若不是从 Termux 会话里启动（例如经由 Termux:API、桌面快捷方式、
//     或某些启动器），进程的 PATH 里可能根本没有 $PREFIX/bin，于是明明装过
//     termux-tools 也会被判成「未安装」，启动时反复弹安装建议弹窗；
//   - 只探测 termux-open-url 也不够：它属于 termux-tools，但用户可能只装了
//     termux-api / 部分命令，探测点应该覆盖「装没装这个包」的代表命令。
//
// 修法：把 $PREFIX/bin 显式并入查找路径，再探测多个代表命令，任一命中即认为已装。
func termuxToolsReady() bool {
	return len(termuxMissingCommands()) == 0
}

// termuxMissingCommands 返回 termux-tools 里当前**找不到**的代表命令。
//
// 空切片 = 已安装。分项回报而不是只给一个 bool，是为了让「没装」和
// 「装了但少某个命令」在错误提示里可区分。
func termuxMissingCommands() []string {
	// 代表命令覆盖三个能力：自动开浏览器、访问手机存储、读写剪贴板。
	probes := []string{"termux-open-url", "termux-storage-get", "termux-setup-storage"}
	var missing []string
	for _, name := range probes {
		if _, err := lookTermuxPath(name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}

// lookTermuxPath 在「进程 PATH + $PREFIX/bin」里查命令。
//
// 显式补 $PREFIX/bin 是关键：Termux 正常会话里 PATH 本来就含它，
// 但从外部拉起 CodeForge 时未必，而 termux-tools 的命令全都装在 $PREFIX/bin。
//
// $PREFIX/bin 这一段直接 stat 而不再 exec.LookPath：LookPath 在 Windows 上只认
// 带扩展名的可执行文件（裸文件永远判「找不到」），而 Termux 是 POSIX 语义 —
// 「存在且可执行」才是那里真正的判据。顺带让这段逻辑在非 Termux 机器上可测。
func lookTermuxPath(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	prefix := os.Getenv("PREFIX")
	if prefix == "" {
		return "", exec.ErrNotFound
	}
	full := filepath.Join(prefix, "bin", name)
	if st, err := os.Stat(full); err == nil && !st.IsDir() {
		return full, nil
	}
	return "", exec.ErrNotFound
}

// termuxToolsHint 生成「termux-tools 状态」的诊断串，供错误提示如实回报。
//
// 用途：曾经把 termux-storage-get 的**任何**失败都写成「需要 termux-tools」，
// 于是用户明明装过包、看到的仍是这句误导的话（真实原因可能是存储未授权、
// SAF 选择器需要前台、或命令不在 PATH）。诊断串把「缺哪个命令」讲清楚。
func termuxToolsHint(err error, out []byte) string {
	missing := termuxMissingCommands()
	if len(missing) > 0 {
		return "termux-tools 未安装或命令不在 PATH（缺少 " + strings.Join(missing, "、") +
			"）。请在 Termux 执行：pkg install termux-tools"
	}
	detail := strings.TrimSpace(string(out))
	if detail == "" {
		detail = err.Error()
	}
	return "termux-tools 已安装，但安卓选择器调用失败：" + clipText(detail, 160) +
		"。若提示没有权限，请在 Termux 执行 termux-setup-storage 授权访问手机存储后重试"
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
	// 复核只看 termux-open-url（自动开浏览器真正依赖的那一个）：探测点越少越不容易
	// 因为某个非关键命令缺失就把整次安装判成失败。
	if err == nil {
		if _, lookErr := lookTermuxPath("termux-open-url"); lookErr != nil {
			err = fmt.Errorf("pkg install 退出正常但未找到 termux-open-url，安装结果异常")
		}
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
