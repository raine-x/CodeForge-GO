package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"codeforge/pkg/platform"
)

// 自定义背景（设置 → 外观 → 新外观）：
//   - GET  /api/appearance            当前配置（background_set / blur / brightness）
//   - POST /api/appearance            保存背景路径 / 模糊 / 亮度（clear=true 清除背景）
//   - GET  /api/appearance/background 服务背景图内容（Content-Type 按扩展名嗅探）
//   - POST /api/appearance/pick       选图：
//       Windows  → PowerShell 打开图片文件对话框，直接返回绝对路径
//       Termux   → termux-storage-get 弹安卓系统选择器，用户选图复制到 dataDir
//       其他     → builtin=true，前端用内置选择器浏览后把路径 POST 回来
//
// 背景图路径持久化在 local.yaml（appearance.background_path），图片内容由
// /api/appearance/background 提供 —— 页面是 http://，不能直接用本地 file:// 路径。

var bgTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".webp": "image/webp", ".gif": "image/gif", ".bmp": "image/bmp",
}

func (s *Server) handleAppearance(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"background_set": s.cfg.Appearance.BackgroundPath != "" &&
				fileExists(s.cfg.Appearance.BackgroundPath),
			"blur":       s.cfg.Appearance.BackgroundBlur,
			"brightness": s.cfg.Appearance.BackgroundBrightness,
		})

	case http.MethodPost:
		var body struct {
			BackgroundPath string `json:"background_path"`
			Blur           *int   `json:"blur"`
			Brightness     *int   `json:"brightness"`
			Clear          bool   `json:"clear"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败: " + err.Error()})
			return
		}
		if body.Clear {
			s.cfg.Appearance.BackgroundPath = ""
		} else if p := strings.TrimSpace(body.BackgroundPath); p != "" {
			if !fileExists(p) {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "背景图文件不存在: " + p})
				return
			}
			s.cfg.Appearance.BackgroundPath = p
		}
		if body.Blur != nil {
			n := *body.Blur
			if n < 0 {
				n = 0
			}
			if n > 40 {
				n = 40
			}
			s.cfg.Appearance.BackgroundBlur = n
		}
		if body.Brightness != nil {
			n := *body.Brightness
			if n < 20 {
				n = 20
			}
			if n > 100 {
				n = 100
			}
			s.cfg.Appearance.BackgroundBrightness = n
		}
		s.saveState(w, "外观设置保存失败")
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         true,
			"background": s.cfg.Appearance.BackgroundPath != "",
			"blur":       s.cfg.Appearance.BackgroundBlur,
			"brightness": s.cfg.Appearance.BackgroundBrightness,
		})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleAppearanceBackground 服务背景图内容。未设置或文件消失时 404，前端据此隐藏背景层。
func (s *Server) handleAppearanceBackground(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	p := s.cfg.Appearance.BackgroundPath
	if p == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ct := bgTypes[strings.ToLower(filepath.Ext(p))]
	if ct == "" {
		ct = http.DetectContentType(data)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleAppearancePick 平台选图。Windows / Termux 直接弹系统选择器；
// Linux 桌面没有可靠的系统图片选择器，走 builtin 让前端用内置选择器选。
func (s *Server) handleAppearancePick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	switch {
	case runtime.GOOS == "windows":
		// 图片过滤器：png/jpg/jpeg/webp/gif/bmp
		path, err := runPSDialog(
			`Add-Type -AssemblyName System.Windows.Forms; ` +
				`$d = New-Object System.Windows.Forms.OpenFileDialog; ` +
				`$d.Filter = '图片|*.png;*.jpg;*.jpeg;*.webp;*.gif;*.bmp'; ` +
				`$d.Title = '选择背景图片'; ` +
				`if ($d.ShowDialog() -ne 'OK') { exit }; Write-Output $d.FileName`)
		s.finishBackgroundPick(w, path, err)

	case platform.IsTermux():
		// 安卓系统选择器：termux-storage-get 会弹 SAF 图片选择器，
		// 用户选中的图片被复制到我们指定的目标路径（扩展名未知 → 统一无后缀，
		// 服务时用 http.DetectContentType 嗅探）。超时 2 分钟（用户找图可能较慢）。
		dest := filepath.Join(s.cfg.DataDir, "background_user")
		_ = os.MkdirAll(filepath.Dir(dest), 0o755)
		_ = os.Remove(dest) // 清掉旧文件，避免用户取消后残留误导
		cmd := exec.Command("termux-storage-get", dest)
		if out, err := cmd.CombinedOutput(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "安卓图片选择失败（需要 termux-tools）: " + strings.TrimSpace(string(out)),
			})
			return
		}
		if !fileExists(dest) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "path": ""}) // 用户取消
			return
		}
		s.finishBackgroundPick(w, dest, nil)

	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "path": "", "builtin": true, "start_path": pickDefaultDir(),
		})
	}
}

// finishBackgroundPick 收尾「背景图选择」：校验 → **落库** → 响应。
//
// ⚠️ 落库这一步曾经缺失，是 2026-09-19 用户报「选完图啥也没有」的根因：
// 只返回 {ok, path} 而不写 cfg.Appearance.BackgroundPath 的话，前端随即去请求
// /api/appearance/background —— 那个接口读的正是这个字段，于是永远 404，
// 界面上一点反应都没有（图层 .on 了，却没有图可画，也不报错）。
//
// 当年 Termux 分支顺手写了 cfg，Windows 分支漏了，所以故障只在 Windows 上暴露；
// 两个分支现在统一走这里，避免再出现「某平台忘了写」。
func (s *Server) finishBackgroundPick(w http.ResponseWriter, path string, err error) {
	p, msg := validatePick(path, err, "背景图")
	if msg != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": msg})
		return
	}
	if p == "" { // 用户取消
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "path": ""})
		return
	}
	if !fileExists(p) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "背景图文件不存在: " + p})
		return
	}
	s.cfg.Appearance.BackgroundPath = p
	if !s.saveState(w, "外观设置保存失败") {
		return
	}
	// background:true 是给前端的「已落库」确认信号（前端据此决定是否补一次显式保存）。
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": p, "background": true})
}

// saveState 把运行状态写回 state.yaml（不再碰用户的 local.yaml）。
// 返回 false 表示已写出 500 响应，调用方必须立即 return
// （否则会二次写 body，产出拼接的坏 JSON）。
func (s *Server) saveState(w http.ResponseWriter, what string) bool {
	if err := s.cfg.SaveState(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": what + ": " + err.Error()})
		return false
	}
	return true
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
