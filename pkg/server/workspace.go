package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"codeforge/pkg/agent"
	"codeforge/pkg/logx"
)

// handleWorkspace 查看 / 切换工作区。
// GET  → {root}
// POST {path} → 校验目录存在后热切换（文件工具 + Agent System Prompt 同步生效），
//
//	并把绝对路径持久化到 config/local.yaml 的 agent.work_dir，
//	重启后自动恢复，不必每次重新选择。path 传空串只清运行态、**不落盘**。
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
		// path 为空 = 清除工作区（回到「未选择」状态），供「新建项目」打开空工作区。
		// 非空时**必须是绝对路径**：别的接口的相对路径语义是「相对工作区根」，但工作区根
		// 没法相对自己 —— 放任相对路径会被 FS.SetRoot 的 filepath.Abs 静默按**进程 CWD**
		// 解析（cf 是项目根、直接跑二进制又可能是别处），换个启动方式工作区就变了。
		if path != "" && !filepath.IsAbs(path) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "工作区必须是绝对路径：" + path,
			})
			return
		}
		// 再校验目录存在 —— 与启动时的检查同源
		//（cmd/agent/main.go：「配置的工作目录不存在或不是目录，按未选择工作区处理」）。
		// 否则一个已删除的目录（或历史遗留的坏分组键）会被静默设成工作目录：
		// 文件工具随后全线报错，用户却看不出是哪一步坏的。
		if path != "" {
			if info, err := os.Stat(path); err != nil || !info.IsDir() {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "目录不存在或不是目录：" + path + "（请重新选择工作区）",
				})
				return
			}
		}
		setter.SetRoot(path)
		wd := s.fs.Root()
		if wd == "" {
			s.agent.SetWorkDir("")
		} else {
			s.agent.SetWorkDir(wd)
			// 只有真正选中目录时才落盘：空串是「新建项目」的**临时**清空语义，
			// 若一并写回，用户只是点一下「新建项目」就会把原先选好的工作区从
			// local.yaml 里抹掉，重启后找不回来。
			s.persistWorkDir(wd)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "root": s.fs.Root()})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// persistWorkDir 把工作区绝对路径写进运行状态（state.yaml 的 agent.work_dir），
// 使选择在重启后依然生效。失败只记日志，不阻断切换本身。
func (s *Server) persistWorkDir(dir string) {
	dir = filepath.Clean(dir)
	s.cfg.Agent.WorkDir = dir
	if err := s.cfg.SaveState(); err != nil {
		logx.Warnf("工作区已切换但写回运行状态失败（重启后需重新选择）: %v", err)
	}
}

// switchSessionWorkspace 把工作区切到该会话**自己**绑定的工作区。
//
// 为什么必须有这一步（用户报告的 bug）：
//
//	点开 RaineOS 项目里的一条会话，让它分析 → 它去分析了 test 项目的文件。
//
// 会话表里本来就存着每条会话的工作区（sessions.workspace），但 `load_session`
// 帧只回放历史消息，从不读它。于是文件工具用的仍是进程当前全局的
// `FS.root` —— 也就是「界面上上次选的那个项目」。
//
// 后果不止「读到别的项目」：
//
//   - 撤销栈、阅读登记（readSeen）都是**工作区级**的，会跨项目混用；
//   - 一个项目的快照里记着另一个项目的文件路径，撤销时写错地方；
//   - 审计日志里「谁读了哪个项目」这条线断了。
//
// 三条纪律：
//
//  1. 会话没绑定工作区（老数据 / 手工建的）时**不动**当前工作区。
//     清空会让用户莫名其妙地丢失当前项目。
//  2. 会话绑定的目录若已不存在，同样不动 —— 报「目录不存在」比静默
//     切到一个错的项目安全得多。
//  3. 切换**不落盘**。这是「点开一条历史会话」，不是「用户选了工作区」；
//     把它写进 state.yaml 会让重启后的工作区变成用户最后点开的那条会话
//     所在的项目，语义完全不对。
//
// 另外一条从上面那条 bug 里学到的：**运行中必须拒绝切换**。
// 工作区是进程全局的，切它会连带改掉正在跑的那个会话所看到的工作区。
// 「A 正在跑、用户顺手点了别的会话」是很自然的操作，不是边缘用法 ——
// 硬切的后果是 A 的下一轮工具调用落到别的项目里，混进分析结果且毫无察觉。
//
// 返回 false = 没有切换（被拒 / 无需切 / 目标不可用）。调用方据此提示用户。
func (s *Server) switchSessionWorkspace(sess *agent.Session) bool {
	if sess == nil {
		return false
	}
	target := strings.TrimSpace(sess.Workspace)
	if target == "" {
		// 会话没绑定工作区：保持现状（纪律 1）
		return false
	}
	if info, err := os.Stat(target); err != nil || !info.IsDir() {
		logx.Warnf("会话 %s 绑定的工作区已不存在，保持当前工作区: %s", sess.ID, target)
		return false // 纪律 2
	}
	target = filepath.Clean(target)
	if cur := s.fs.Root(); cur == target {
		// 同一个工作区 —— 什么都不用做。
		//
		// 这条同时也是「同一项目里两个会话并发」的关键：它们工作区相同，
		// 切会话时压根不碰 FS.root，因此「有别的会话在跑」与这里无关。
		// 曾在下面加一条「任何会话在跑就拒绝切换」，结果同一项目里
		// 第二个会话根本点不开 —— 并发被自己堵死了。
		return true
	}

	// 跨项目：工作区是**进程全局**的，切它会连带改掉正在跑的那些会话
	// 所看到的工作区（A 的下一轮工具调用会落到 B 的项目里，混进分析结果
	// 且毫无察觉）。所以这里必须挡住。
	//
	// ⚠️ 这条限制是「工作区还没归会话」的历史包袱，不是设计意图。
	// 2.2（Workspace 结构化 + 工作区下沉到会话）落地后就可以删掉，
	// 届时每个会话有自己的工作区，跨项目并发与同项目并发一样自然。
	if s.anySessionRunning() {
		logx.Warnf("有会话正在运行，暂不切工作区到 %s（等 2.2 工作区归会话后可解除）", target)
		return false
	}

	setter, ok := s.fs.(interface{ SetRoot(string) })
	if !ok {
		return false
	}
	setter.SetRoot(target)
	// FS 与 Agent 是两份独立状态，只切一个就是历史上那种「一半修法」。
	s.agent.SetWorkDir(target)
	// 刻意不调 persistWorkDir —— 纪律 3
	logx.Infof("切到会话 %s 的工作区：%s", sess.ID, target)
	return true
}

// anySessionRunning 报告当前是否有任何会话在跑。
//
// 只用于「跨项目切工作区」这一个决策点。同项目切换在
// switchSessionWorkspace 的开头就 return 了，不受这里影响。
func (s *Server) anySessionRunning() bool {
	return len(s.agent.RunningSessionIDs()) > 0
}

// handleNotifyPrefs 查看 / 设置「任务完成系统通知」偏好。
// GET  → {enabled, min_interval_ms}
// POST {enabled} → 热更新并写回 config/local.yaml（重启后保持）。
//
// 通知只在窗口不可见（切走标签页 / 最小化）时发送，且受最小间隔节流，
// 避免「什么都没做却弹出任务已完成」这类打扰。
func (s *Server) handleNotifyPrefs(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":         s.cfg.NotifyEnabled(),
			"min_interval_ms": s.cfg.NotifyMinInterval().Milliseconds(),
		})

	case http.MethodPost:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
			return
		}
		if body.Enabled == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 enabled 字段"})
			return
		}
		on := *body.Enabled
		s.cfg.Notify.Enabled = &on
		if err := s.cfg.SaveState(); err != nil {
			logx.Warnf("通知开关已生效但写回运行状态失败（重启后需重新设置）: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": s.cfg.NotifyEnabled()})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handlePickFolder 平台相关的文件夹选择入口：
//   - Windows：弹出 PowerShell/.NET 资源管理器选择对话框（失败直接报错，不回退内置选择器）；
//   - Linux/macOS/Termux：返回内置选择器的起始路径（"builtin":true）= 用户主目录 ~。
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

// handleStageFile 把工作区外的文件复制一份进工作区（attachments/ 目录）。
// POST {path} → {ok, staged_path(相对工作区), name}
// 用途：@工作区外文件 时，代理默认读不到区外内容；先 stage 一份副本到
// 工作区内，模型通过副本路径操作。副本命名保持原文件名（同名加序号防覆盖）。
func (s *Server) handleStageFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体解析失败"})
		return
	}
	src := strings.TrimSpace(body.Path)
	if src == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path 不能为空"})
		return
	}
	root := s.fs.Root()
	if root == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "还没有选择工作区"})
		return
	}

	// ⚠️ 相对路径按**工作区根**解析（与文件工具 FS.Resolve 的约定一致：「相对路径基于工作区根」），
	// 不能落到进程 CWD。否则 `@sub/a.go` 会去「进程启动目录」找同名文件：
	// 找不到就报「文件不存在」这种莫名其妙的错；**恰好找到，就把另一个文件静默拷进
	// attachments/**，模型读到的东西完全不是用户指的那个。
	if !filepath.IsAbs(src) {
		src = filepath.Join(root, src)
	}

	// 只 stage 工作区外的文件；区内文件模型本来就看得见，无需副本。
	// 词法判断即可：这里只是分流「要不要 copy」，真正的越权拦截在文件工具层。
	if pathWithin(root, src) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inside": true, "staged_path": filepath.ToSlash(src), "name": filepath.Base(src)})
		return
	}

	info, err := os.Stat(src)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "路径不存在: " + src})
		return
	}

	// 目录：整棵复制进 attachments/<目录名>/（保持内部结构）。
	if info.IsDir() {
		stagedRoot, count, serr := stageDirectory(root, src)
		if serr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "暂存文件夹失败: " + serr.Error()})
			return
		}
		logx.Infof("已暂存工作区外文件夹：%s → %s（%d 个文件）", src, stagedRoot, count)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "inside": false, "is_dir": true, "count": count,
			"staged_path": stagedRoot, "name": filepath.Base(src),
		})
		return
	}

	// 副本目录：attachments（不走 .codeforge —— 那是隐藏目录，会被 tree/搜索排除）
	dstDir := filepath.Join(root, "attachments")
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "创建附件目录失败: " + err.Error()})
		return
	}

	// 同名防覆盖：存在同名不同源文件时追加 -1 / -2 序号
	name := filepath.Base(src)
	dst := filepath.Join(dstDir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); err != nil {
			break // 不存在：可用
		}
		// 已存在：若内容与源一致（重复 stage 同一文件）直接复用，不再复制
		if sameFile(src, dst) {
			rel, _ := filepath.Rel(root, dst)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "inside": false,
				"staged_path": filepath.ToSlash(rel), "name": name, "reused": true,
			})
			return
		}
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		dst = filepath.Join(dstDir, name)
	}

	data, err := os.ReadFile(src)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取源文件失败: " + err.Error()})
		return
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "写入副本失败: " + err.Error()})
		return
	}
	rel, _ := filepath.Rel(root, dst)
	logx.Infof("已暂存工作区外文件：%s → %s", src, rel)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "inside": false,
		"staged_path": filepath.ToSlash(rel), "name": name,
	})
}

// stageDirectory 把工作区外的目录整棵复制到 工作区/attachments/<目录名>/，
// 保持内部相对结构。返回（相对工作区根的正斜杠路径, 复制的文件数, error)。
//
// 与单文件 stage 同一约定：attachments 不用 .codeforge（隐藏目录会被 tree/搜索
// 排除）；同名文件若内容一致则复用、不重复复制；软链接与非普通文件一律跳过
// （避免把指向区外的链接拷进来）。目录过大时截断并返回部分结果，防止误把整个
// 磁盘塞进工作区。
func stageDirectory(root, src string) (string, int, error) {
	const (
		maxFiles = 2000      // 单个文件夹最多复制的文件数
		maxBytes = 512 << 20 // 累计字节上限（512MiB）
		maxDepth = 32        // 目录嵌套深度上限
	)
	base := filepath.Base(src)
	dstRoot := filepath.Join(root, "attachments", base)
	// 目标根目录同名防覆盖：已存在且非空时追加 -1/-2 序号。
	for i := 1; ; i++ {
		fi, err := os.Stat(dstRoot)
		if err != nil {
			break // 不存在：可用
		}
		if fi.IsDir() {
			if entries, _ := os.ReadDir(dstRoot); len(entries) == 0 {
				break // 空目录可直接复用
			}
		}
		dstRoot = filepath.Join(root, "attachments", base+"-"+strconv.Itoa(i))
	}

	count := 0
	total := int64(0)
	truncated := false
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		depth := len(strings.Split(rel, string(filepath.Separator)))
		if d.IsDir() {
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dstRoot, rel), 0o755)
		}
		if !d.Type().IsRegular() { // 跳过软链接、设备、管道等
			return nil
		}
		if count >= maxFiles || total >= maxBytes {
			truncated = true
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		dst := filepath.Join(dstRoot, rel)
		if sameFile(p, dst) { // 同名同内容复用（重复 stage 幂等）
			count++
			total += info.Size()
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		count++
		total += info.Size()
		return nil
	})
	if err != nil {
		return "", count, err
	}
	relRoot, _ := filepath.Rel(root, dstRoot)
	out := filepath.ToSlash(relRoot)
	if truncated {
		return out, count, fmt.Errorf("文件夹过大，已截断（最多 %d 个文件 / %d MiB）", maxFiles, maxBytes>>20)
	}
	return out, count, nil
}

// sameFile 粗比较两个文件是否同一内容（大小一致且字节相同）。
func sameFile(a, b string) bool {
	ia, ea := os.Stat(a)
	ib, eb := os.Stat(b)
	if ea != nil || eb != nil || ia.Size() != ib.Size() {
		return false
	}
	da, err := os.ReadFile(a)
	if err != nil {
		return false
	}
	db, err := os.ReadFile(b)
	if err != nil {
		return false
	}
	return bytes.Equal(da, db)
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

// validatePick 校验对话框返回值，返回 (清理后的路径, 错误文案)。
// 错误文案非空 = 应报 500；路径为空且无错误 = 用户取消。
//
// 为什么非法 UTF-8 要报错而不是照用：损坏的工作区路径会一路写进会话的 workspace 键，
// 再被重命名/新建会话反复放大（见 memory 里那次 '????' 事故）。
func validatePick(path string, err error, what string) (string, string) {
	if err != nil {
		// 用户取消（退出码 -1073741510 / Ctrl-C 类）以外的情况都视为对话框失败
		return "", "打开系统对话框失败: " + err.Error()
	}
	if path != "" && !utf8.ValidString(path) {
		return "", "所选" + what + "路径不是合法 UTF-8（PowerShell 输出编码异常）：请把该" + what + "改成纯英文名后重试"
	}
	return path, ""
}

// finishPick 统一收尾：对话框失败 → 500；路径非法 UTF-8 → 500；正常 → {ok, path}。
// 注意它**不落库** —— 需要持久化的场景（如背景图）请用 finishBackgroundPick。
func (s *Server) finishPick(w http.ResponseWriter, path string, err error, what string) {
	p, msg := validatePick(path, err, what)
	if msg != "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": p != "", "path": p})
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

// pickDefaultDir 计算 Linux/macOS/Termux 内置选择器的默认起始目录：直接挂 ~。
//
// 之前 Termux 默认跳 ~/storage/shared（授权异常时还会自动跑 termux-setup-storage），
// 但 shared 权限不在位时整个浏览列表就空了（安卓「选择工作目录为空」的成因之一）。
// 改为直接挂 ~：~ 一定能列出；授权过存储后 storage/shared 软链就在 ~ 下，点进去即可。
func pickDefaultDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "/"
	}
	return home
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
