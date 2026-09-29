// Package backend 的本地实现。
package backend

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"codeforge/pkg/platform"
)

// Local 是本机文件系统的 Backend 实现。
//
// 它是**纯 IO**：不知道「工作区」是什么，也不做任何越界判断。
// 围栏在 builtin.Guard，那是唯一一份实现。
type Local struct{}

// NewLocal 构造本机后端。
func NewLocal() *Local { return &Local{} }

var _ Backend = (*Local)(nil)

// ReadFile 读整个文件。
func (l *Local) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// WriteFile 原子覆盖写：同目录临时文件 → 写 → Sync → Chmod → rename。
//
// 必须是原子的：「打开→截断→写」在进程写一半被杀时留下半截文件，
// 而读侧会看到那个半截版本。
func (l *Local) WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".cf-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 任何失败路径都要清掉临时文件，否则目录里会积累垃圾。
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp 用 0600 建文件；显式改成目标权限，否则产物权限会莫名变严。
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	// 同目录是必须的：跨文件系统 rename 会失败。
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // 已消费，defer 不再删
	return nil
}

// Stat 是 stat 语义（**跟随**软链）。
func (l *Local) Stat(path string) (Info, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	return Info{Name: filepath.Base(path), IsDir: fi.IsDir(), Size: fi.Size()}, nil
}

// List 列目录。递归与���递归的失败语义**不同**，见类型注释。
func (l *Local) List(dir string, opts ListOptions) ([]Entry, error) {
	skip := opts.SkipDirs
	if skip == nil {
		skip = DefaultSkipDirs
	}

	if !opts.Recursive {
		// 非递归：根读不了直接报错。
		des, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		out := make([]Entry, 0, len(des))
		for _, de := range des {
			// 单条 Info() 失败被跳过（不因一条坏条目让整次列举失败）。
			info, err := de.Info()
			if err != nil {
				continue
			}
			out = append(out, Entry{
				Path: filepath.Join(dir, de.Name()),
				Info: Info{Name: de.Name(), IsDir: de.IsDir(), Size: info.Size()},
			})
		}
		return out, nil
	}

	// 递归：逐条吞错，连 walkErr 都丢弃 ⇒ 根不存在时返回空列表 + nil。
	var out []Entry
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		// 递归不产出根自身。
		if path == dir {
			return nil
		}
		if info.IsDir() {
			if skip[info.Name()] {
				return filepath.SkipDir
			}
			out = append(out, Entry{
				Path: path,
				Info: Info{Name: info.Name(), IsDir: true},
			})
			return nil
		}
		out = append(out, Entry{
			Path: path,
			Info: Info{Name: info.Name(), IsDir: false, Size: info.Size()},
		})
		return nil
	})
	return out, nil
}

// Remove 删单个文件（目录仅在为空时可删，与 os.Remove 一致）。
func (l *Local) Remove(path string) error { return os.Remove(path) }

// MkdirAll 建目录及全部父级。
func (l *Local) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// Realpath 解析软链。**逐字复刻**逐级向上的语义。
//
// 为什么不能简化成 filepath.EvalSymlinks(path)：新建文件的目标不存在，
// 直接解析必然失败，于是「区内软链 A → 区外，通过 A 写一个区外的新文件」
// 这个绕过就漏了。逐级向上找「第一个能完整解析的祖先」再拼回尾部，
// 才能让 A 本身被解析出来，从而被围栏判定为区外。
func (l *Local) Realpath(path string) (string, error) {
	suffix := ""
	cur := path
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if suffix == "" {
				return real, nil
			}
			return filepath.Join(real, suffix), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// 已到文件系统根仍解析失败。
			// ⚠️ 此处**不**返回原路径充数，而是报错 ——
			// 「解析不出来」与「解析出来就是这个」必须可区分，
			// 否则围栏无法判断该不该放行。调用方（Guard）决定 fail-closed 策略。
			return "", &PathNotResolvableError{Path: path}
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

// PathNotResolvableError 表示 Realpath 到根仍无法解析。
//
// 单列一个类型而不是复用 os.ErrNotExist：前者是「这条路径的真实位置
// 我无法确定」（可能整条链都没权限），后者是「确定地不存在」。
// 围栏对前者的处置（拒绝）与后者（放行，走新建路径）不同。
type PathNotResolvableError struct{ Path string }

func (e *PathNotResolvableError) Error() string {
	return "无法确定路径的真实位置（逐级向上到根仍未解析成功）: " + e.Path
}

// Grep 按内容检索。
//
// Pattern 是**纯子串**而非正则；与既有实现一致。
func (l *Local) Grep(ctx context.Context, root string, opts GrepOptions) (GrepResult, error) {
	skip := opts.SkipDirs
	if skip == nil {
		skip = DefaultSkipDirs
	}
	needle := opts.Pattern
	if !opts.CaseSensitive {
		needle = strings.ToLower(needle)
	}

	var res GrepResult
	maxFileSize := opts.MaxFileSize

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if path != root && skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(res.Matches) >= opts.MaxResults {
			res.Truncated = true
			return filepath.SkipAll
		}
		if opts.Glob != "" {
			// 错误吞掉等同「不匹配」，与既有实现一致。
			if ok, _ := filepath.Match(opts.Glob, info.Name()); !ok {
				return nil
			}
		}
		if info.Size() > maxFileSize {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), int(maxFileSize))
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			haystack := line
			if !opts.CaseSensitive {
				haystack = strings.ToLower(line)
			}
			if strings.Contains(haystack, needle) {
				res.Matches = append(res.Matches, Match{
					Path: path, Line: lineNo, Text: strings.TrimRight(line, "\r"),
				})
				if len(res.Matches) >= opts.MaxResults {
					break
				}
			}
		}
		res.Scanned++
		return nil
	})
	// ⚠️ 截断（SkipAll）不是错误，必须返回 nil。
	if walkErr != nil && walkErr != filepath.SkipAll {
		return GrepResult{}, walkErr
	}
	return res, nil
}

// Glob 按文件名模式查找。路径是**相对 root** 的。
func (l *Local) Glob(ctx context.Context, root, pattern string, opts GlobOptions) (GlobResult, error) {
	skip := opts.SkipDirs
	if skip == nil {
		skip = DefaultSkipDirs
	}
	var res GlobResult

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if path != root && skip[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if len(res.Paths) >= opts.MaxResults {
			res.Truncated = true
			return filepath.SkipAll
		}
		if ok, _ := filepath.Match(pattern, info.Name()); ok {
			rel, _ := filepath.Rel(root, path)
			res.Paths = append(res.Paths, rel)
		}
		// ⚠️ 计数点在命中判断**之外** —— 口径是「路过」，
		// 与 Grep 的「扫描过」不同。别顺手统一。
		res.Scanned++
		return nil
	})
	if walkErr != nil && walkErr != filepath.SkipAll {
		return GlobResult{}, walkErr
	}
	return res, nil
}

// Exec 执行一条 shell 命令。
//
// ⚠️ 必须用 exec.CommandContext 而**不是** exec.Command：不是为了它的
// 自动 Kill，而是为了让 os/exec 在 Start 里建立 watchCtx goroutine ——
// cmd.WaitDelay 的「ctx 到期后关管道」逻辑只认 c.ctx。换成 Command
// 就没有人 watch，超时路径下 WaitDelay 计时器根本不启动。
//
// ⚠️ cmd.WaitDelay 不可省：只杀进程组而不设它，孙进程攥着管道写端会让
// cmd.Wait() 永久阻塞（工具根本不返回），等于把「进程泄漏」换成
// 「goroutine 泄漏 + 超时语义失效」。
func (l *Local) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	shell, shellArgs := platform.Shell()
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	argv := append(shellArgs, req.Command)
	cmd := exec.CommandContext(cctx, shell, argv...)
	cmd.Dir = req.Dir
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	cmd.WaitDelay = platform.ProcessWaitDelay

	res := ExecResult{Shell: shell}
	// StartGrouped 把 Start 一起做：Unix 的 Setpgid 必须在 Start **前**设，
	// Windows 的 AssignProcessToJobObject 必须在 Start **后**做，
	// 两边时序相反，调用方无法自己统一。覆盖 cmd.Cancel 也在其中完成。
	grp, err := platform.StartGrouped(cmd)
	if err != nil {
		res.StartErr = err
		res.ExitCode = -1
		return res, nil
	}
	// KILL_ON_JOB_CLOSE 意味着关句柄即终止组内所有进程 ——
	// 正常执行完毕也要调，否则 start /b、nohup 之类的后台孙进程会驻留。
	defer platform.CloseGroup(grp)

	start := time.Now()
	waitErr := cmd.Wait()
	res.Duration = time.Since(start)

	switch {
	case waitErr == nil:
		res.ExitCode = 0
	case cctx.Err() == context.DeadlineExceeded:
		// ⚠️ 必须是 `== DeadlineExceeded` 而不是 `!= nil`：
		// 调用方 ctx 被取消（用户点停止）与「超时」是两件事，
		// 前者不该报成超时。既有实现用的就是精确比对。
		res.ExitCode = -2
		res.TimedOut = true
	default:
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			res.ExitCode = -1
		}
	}
	res.Output = buf.String()
	return res, nil
}
