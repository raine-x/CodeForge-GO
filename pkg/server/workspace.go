package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

// handleWorkspace 查看 / 切换工作区。
// GET  → {root}
// POST {path} → 校验目录存在后热切换（文件工具 + Agent System Prompt 同步生效），
//
//	并把绝对路径持久化到 config/local.yaml 的 agent.work_dir，
//	重启后自动恢复，不必每次重新选择。
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"root": s.fs.Root()})

	case http.MethodPost:
		var body struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		path := strings.TrimSpace(body.Path)
		setter, ok := s.fs.(interface{ SetRoot(string) })
		if !ok {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "当前文件系统不支持切换工作区"})
			return
		}
		// path 为空 = 清除工作区（回到「未选择」状态），供「新建对话」打开空工作区。
		setter.SetRoot(path)
		wd := s.fs.Root()
		if wd == "" {
			s.agent.SetWorkDir("")
		} else {
			s.agent.SetWorkDir(wd)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "root": s.fs.Root()})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// persistWorkDir 把工作区绝对路径写进 config/local.yaml（agent.work_dir），
// 使选择在重启后依然生效。失败只记日志，不阻断切换本身。
func (s *Server) persistWorkDir(dir string) {
	dir = filepath.Clean(dir)
	s.cfg.Agent.WorkDir = dir
	if cfgDir := s.cfg.ConfigDir(); cfgDir != "" {
		if err := s.cfg.Save(filepath.Join(cfgDir, "local.yaml")); err != nil {
			log.Printf("警告：工作区已切换但写回配置失败（重启后需重新选择）: %v", err)
		}
	}
}

// handlePickFolder 平台相关的文件夹选择入口：
//   - Windows：弹出 PowerShell/.NET 资源管理器选择对话框（失败直接报错，不回退内置选择器）；
//   - Linux/macOS：返回内置选择器的默认起始路径（"builtin":true）：
//     Termux 默认 ~/storage/shared（不存在/无权限时自动运行一次 termux-setup-storage
//     申请存储权限后重试，仍失败退回 ~）；其余 Linux/macOS 默认 ~。
func (s *Server) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if runtime.GOOS == "windows" {
		s.pickFolderWindows(w)
		return
	}

	// 非 Windows：内置选择器 + 平台默认起始目录
	defaultDir := pickDefaultDir()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"path":       "",
		"builtin":    true,
		"start_path": defaultDir,
		"platform":   runtime.GOOS,
	})
}

// handlePickFile 选一个文件（输入区 ＋ → 添加文件）。
//   - Windows：资源管理器「打开文件」对话框（PowerShell OpenFileDialog），直接返回绝对路径；
//   - 其他平台（Linux/Termux 等）：没有原生对话框，返回 builtin=true + 起始目录，
//     由前端用内置选择器浏览（与选工作区同一套）。
func (s *Server) handlePickFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if runtime.GOOS == "windows" {
		s.pickFileWindows(w)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"path":       "",
		"builtin":    true,
		"start_path": pickDefaultDir(),
		"platform":   runtime.GOOS,
	})
}

// psUTF8 是每段 PowerShell 对话框脚本都必须带的编码前置语句。
//
// ⚠️ PowerShell 5.1 在 stdout 被重定向时用**控制台代码页**（中文 Windows 上是 gb2312/936）
// 编码输出，于是 `Write-Output $d.SelectedPath` 会吐出 GB2312 字节，Go 按 UTF-8 读就成了
// 非法字符串 —— 用户选中任何含中文/日文/带音标字符的路径都会被静默损坏。
// （实测：'C:\Users\26536\Desktop\测试' → 尾部 8 字节 b2e2cad4cfeec4bf；
//
//	加上下面这句后变成合法的 e6b58be8af95。）
//
// **新增任何 exec.Command("powershell", …) 取字符串的地方都要带上它。**
const psUTF8 = `try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}; `

// runPSDialog 跑一段 PowerShell 对话框脚本并取回用户所选路径（空串 = 用户取消）。
// 编码前置语句由这里统一加，调用方只写对话框本身。
func runPSDialog(body string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-STA", "-Command", psUTF8+body).Output()
	if err != nil {
		// 用户取消（退出码 -1073741510 / Ctrl-C 类）以外的情况都视为对话框失败
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// finishPick 统一收尾：对话框失败 → 500；路径非法 UTF-8 → 500（宁可报错也不放损坏的路径过去）；
// 正常 → {ok, path}，用户取消时 ok=false 且 path 为空。
//
// 为什么非法 UTF-8 要报错而不是照用：损坏的工作区路径会一路写进会话的 workspace 键，
// 再被重命名/新建会话反复放大（见 memory 里那次 '????' 事故）。
func (s *Server) finishPick(w http.ResponseWriter, path string, err error, what string) {
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "打开系统对话框失败: " + err.Error()})
		return
	}
	if path != "" && !utf8.ValidString(path) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "所选" + what + "路径不是合法 UTF-8（PowerShell 输出编码异常）：请把该" + what + "改成纯英文名后重试",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": path != "", "path": path})
}

// pickFolderWindows 通过 PowerShell 调用 .NET FolderBrowserDialog（零 CGO）。
func (s *Server) pickFolderWindows(w http.ResponseWriter) {
	path, err := runPSDialog(`Add-Type -AssemblyName System.Windows.Forms; ` +
		`$d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
		`$d.Description = '选择工作区文件夹'; ` +
		`$d.ShowNewFolderButton = $true; ` +
		`if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $d.SelectedPath }`)
	s.finishPick(w, path, err, "目录")
}

// pickFileWindows 通过 PowerShell 调用 .NET OpenFileDialog（资源管理器「打开文件」对话框）。
func (s *Server) pickFileWindows(w http.ResponseWriter) {
	path, err := runPSDialog(`Add-Type -AssemblyName System.Windows.Forms; ` +
		`$d = New-Object System.Windows.Forms.OpenFileDialog; ` +
		`$d.Title = '选择要添加的文件'; ` +
		`$d.Multiselect = $false; ` +
		`$d.CheckFileExists = $true; ` +
		`if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $d.FileName }`)
	s.finishPick(w, path, err, "文件")
}

// pickDefaultDir 计算 Linux/macOS 内置选择器的默认起始目录：
// Termux（PREFIX 含 com.termux）→ ~/storage/shared，失败逐级退回 ~；
// 其他系统 → ~。
func pickDefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "/"
	}
	if !isTermux() {
		return home
	}

	shared := filepath.Join(home, "storage", "shared")
	if dirAccessible(shared) {
		return shared
	}
	// 目录不存在或无权限：申请一次存储权限（termux-setup-storage 会弹授权框并建软链）
	if setupTermuxStorage(home) && dirAccessible(shared) {
		return shared
	}
	return home // 最终退回 ~
}

// isTermux 判断是否运行在 Termux 环境（$PREFIX 指向 com.termux）。
func isTermux() bool {
	return strings.Contains(os.Getenv("PREFIX"), "com.termux")
}

// dirAccessible 目录存在且可列出内容。
func dirAccessible(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && entries != nil
}

// setupTermuxStorage 运行一次 termux-setup-storage 申请存储权限。
// 该命令会弹出 Android 授权对话框并在用户同意后创建 ~/storage 软链；
// 限时等待（授权框可能停留较久），失败不阻塞后续回退。
func setupTermuxStorage(home string) bool {
	cmd := exec.Command("termux-setup-storage")
	if err := cmd.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		// 授权框仍挂着：不杀进程（用户可能稍后点同意），先按失败处理走回退
		return false
	}
	// 授权成功后 storage 软链立即可用；给文件系统一点落地时间
	for i := 0; i < 10; i++ {
		if dirAccessible(filepath.Join(home, "storage", "shared")) {
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}
