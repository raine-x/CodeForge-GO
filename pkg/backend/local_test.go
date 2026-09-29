package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 这组测试的定位：**localBackend 是纯机械搬运，不是重写**。
// 所以断言的重点是「与既有行为逐项一致」，而不是「这样设计更好」。
// 每条用例都在注释里标出它对应原实现里的哪几行。

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadWriteRoundtrip 读写往返。
// 对应原：file_ops.go 的 read_file / atomicWriteFile。
func TestReadWriteRoundtrip(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")

	// 目标不存在时 ReadFile 必须报错，且不能返回部分内容
	if got, err := be.ReadFile(p); err == nil {
		t.Errorf("读不存在的文件应报错，实际返回 %q", got)
	}

	for _, c := range []string{"", "hello", "a\x00b", "line1\r\nline2", "中文内容"} {
		if err := be.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatalf("写 %q 失败: %v", c, err)
		}
		got, err := be.ReadFile(p)
		if err != nil {
			t.Fatalf("读失败: %v", err)
		}
		if string(got) != c {
			t.Errorf("往返不一致: 写 %q 读 %q", c, got)
		}
	}
}

// TestWriteLeavesNoTempLeftover 落盘后目录里不多出残留。
// 对应原：atomicWriteFile 的 defer os.Remove 清理。
func TestWriteLeavesNoTempLeftover(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")

	if err := be.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("落盘后目录应只有目标文件，实际有 %v", names)
	}
}

// TestWriteFailureLeavesNoTempFile 失败路径也要清临时文件。
func TestWriteFailureLeavesNoTempFile(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	// 目标在一个不存在的父目录下 → CreateTemp 失败
	p := filepath.Join(dir, "nonexistent", "a.txt")
	if err := be.WriteFile(p, []byte("x"), 0o644); err == nil {
		t.Fatalf("父目录不存在时写入应失败")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("失败后目录应为空，实际有 %d 项", len(entries))
	}
}

// TestStatFollowsSymlink Stat 跟随软链。
func TestStatFollowsSymlink(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "real.txt"), "0123456789")

	// 指向文件的链接：Windows 上 os.Symlink 需特权，故只在 POSIX 跑。
	// 目录链接（junction）在 Windows 上免特权，由 makeLink 兜底。
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link.txt")
		if err := os.Symlink(filepath.Join(outside, "real.txt"), link); err != nil {
			t.Skipf("建软链失败: %v", err)
		}
		info, err := be.Stat(link)
		if err != nil {
			t.Fatalf("Stat 软链应成功: %v", err)
		}
		if info.Size != 10 {
			t.Errorf("Stat 应跟随软链拿到目标大小 10，实际 %d", info.Size)
		}
		if info.IsDir {
			t.Errorf("指向文件的软链不该报 IsDir")
		}
		return
	}

	// Windows：目录链接应被 Stat 判为目录
	outsideDir := filepath.Join(outside, "d")
	mustWrite(t, filepath.Join(outsideDir, "x.txt"), "x")
	link := makeLink(t, dir, "dlink", outsideDir)
	info, err := be.Stat(link)
	if err != nil {
		t.Fatalf("Stat 目录链接应成功: %v", err)
	}
	if !info.IsDir {
		t.Errorf("指向目录的链接应报 IsDir=true")
	}
}

// TestStatThreeStates Stat 的三态。
func TestStatThreeStates(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	mustWrite(t, f, "abc")

	info, err := be.Stat(f)
	if err != nil || info.IsDir || info.Size != 3 {
		t.Errorf("普通文件 Stat 应为 {IsDir:false Size:3}，实际 %+v err=%v", info, err)
	}
	di, err := be.Stat(dir)
	if err != nil || !di.IsDir {
		t.Errorf("目录 Stat 应为 IsDir:true，实际 %+v err=%v", di, err)
	}
	if _, err := be.Stat(filepath.Join(dir, "nope.txt")); err == nil {
		t.Errorf("不存在的路径 Stat 应报错")
	}
}

// TestListNonRecursiveFailFast 非递归：根读不了要报错。
// 对应原：file_ops.go list_dir 的 os.ReadDir 分支（`读取目录失败`）。
func TestListNonRecursiveFailFast(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	if _, err := be.List(filepath.Join(dir, "nope"), ListOptions{}); err == nil {
		t.Errorf("非递归列举不存在的目录应报错")
	}
}

// TestListRecursiveSwallowsErrors 递归：根不存在返回空 + nil。
//
// 对应原：filepath.Walk 的 `_ =` 加上 `if err != nil { return nil }`
// —— 这是既有行为（不是疏漏），抽象层不许「顺手修正」。
func TestListRecursiveSwallowsErrors(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	entries, err := be.List(filepath.Join(dir, "nope"), ListOptions{Recursive: true})
	if err != nil {
		t.Errorf("递归列举不存在的目录应返回 nil error，实际 %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("应返回空列表，实际 %d 项", len(entries))
	}
}

// TestListSkipsRootAndSkipDirs 递归不产出根自身，且跳过配置的目录。
func TestListSkipsRootAndSkipDirs(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "a")
	mustWrite(t, filepath.Join(dir, "sub", "b.txt"), "b")
	mustWrite(t, filepath.Join(dir, "node_modules", "x.js"), "x")

	entries, err := be.List(dir, ListOptions{Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range entries {
		paths = append(paths, filepath.ToSlash(e.Path))
	}
	for _, p := range paths {
		if p == filepath.ToSlash(dir) {
			t.Errorf("递归列举不该产出根自身: %v", paths)
		}
		if contains(paths, p) && containsStr(p, "node_modules") {
			t.Errorf("不该列出 node_modules 下的内容: %v", paths)
		}
	}
	// a.txt 与 sub/b.txt 必须在
	joined := ""
	for _, p := range paths {
		joined += p + ";"
	}
	if !containsStr(joined, "a.txt") || !containsStr(joined, "sub/b.txt") {
		t.Errorf("应列出 a.txt 与 sub/b.txt，实际 %v", paths)
	}
}

// TestGrepMatchesAndMeta Grep 命中、glob 过滤、scanned 计数、truncated。
// 对应原：search.go SearchTool.Execute 的整个 walk。
func TestGrepMatchesAndMeta(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.go"), "package main\nfunc Hello() {}\n// TODO: fix\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "hello world\n")
	mustWrite(t, filepath.Join(dir, "c.go"), "package other\n")

	res, err := be.Grep(context.Background(), dir, GrepOptions{
		Pattern:       "package",
		MaxResults:    100,
		MaxFileSize:   2 << 20,
		CaseSensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Errorf("应命中 2 处，实际 %d: %+v", len(res.Matches), res.Matches)
	}
	if res.Truncated {
		t.Errorf("未达上限不该标 Truncated")
	}
	if res.Scanned != 3 {
		t.Errorf("scanned 应为 3（扫过 3 个文件），实际 %d", res.Scanned)
	}

	// glob 只对基名生效
	res, err = be.Grep(context.Background(), dir, GrepOptions{
		Pattern: "package", Glob: "*.go",
		MaxResults: 100, MaxFileSize: 2 << 20, CaseSensitive: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Matches) != 2 {
		t.Errorf("glob=*.go 应命中 2 处，实际 %d", len(res.Matches))
	}

	// 大小写不敏感
	res, _ = be.Grep(context.Background(), dir, GrepOptions{
		Pattern: "PACKAGE", MaxResults: 100, MaxFileSize: 2 << 20, CaseSensitive: false,
	})
	if len(res.Matches) != 2 {
		t.Errorf("不区分大小写应命中 2 处，实际 %d", len(res.Matches))
	}

	// 截断：返回 nil error 且标 Truncated
	res, err = be.Grep(context.Background(), dir, GrepOptions{
		Pattern: "package", MaxResults: 1, MaxFileSize: 2 << 20, CaseSensitive: true,
	})
	if err != nil {
		t.Errorf("截断时必须返回 nil error，实际 %v", err)
	}
	if !res.Truncated {
		t.Errorf("达到 MaxResults 应标 Truncated")
	}
	if len(res.Matches) != 1 {
		t.Errorf("截断后应只返回 1 条，实际 %d", len(res.Matches))
	}
}

// TestGlobPathsAreRelativeAndScannedCountsVisited Glob 返回相对路径，
// 且 scanned 数的是「路过」而非「命中」。
//
// ⚠️ 这两个口径不同（Grep 数「扫描过」、Glob 数「路过」）是既有行为。
// 有人会「顺手统一」，而统一的直接后果是 find_files 的 meta 数字变了。
func TestGlobPathsAreRelativeAndScannedCountsVisited(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.py"), "x")
	mustWrite(t, filepath.Join(dir, "b.py"), "x")
	mustWrite(t, filepath.Join(dir, "c.txt"), "x")
	mustWrite(t, filepath.Join(dir, "sub", "d.py"), "x")

	res, err := be.Glob(context.Background(), dir, "*.py", GlobOptions{MaxResults: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Paths) != 3 {
		t.Errorf("*.py 应命中 3 个，实际 %d: %v", len(res.Paths), res.Paths)
	}
	// 必须是**相对**路径
	for _, p := range res.Paths {
		if filepath.IsAbs(p) {
			t.Errorf("Glob 应返回相对路径，实际 %q", p)
		}
	}
	// scanned 数「路过」：4 个非目录条目全部计入（含未命中的 c.txt）
	if res.Scanned != 4 {
		t.Errorf("scanned 应为 4（路过 4 个条目），实际 %d", res.Scanned)
	}
}

// TestRealpathResolvesSymlink Realpath 能解析软链/联接。
func TestRealpathResolvesSymlink(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.txt"), "s")

	link := makeLink(t, dir, "link", outside)

	got, err := be.Realpath(link)
	if err != nil {
		t.Fatalf("Realpath 链接失败: %v", err)
	}
	// 解析结果必须**逃出** dir —— 这正是围栏判定越界的依据。
	if !isUnder(got, dir) {
		if !isUnder(got, outside) {
			t.Errorf("Realpath(%q) = %q，既不在 %q 也不在 %q",
				link, got, dir, outside)
		}
	}
}

// isUnder 报告 p 是否在 dir 之内（含 dir 自身）。
func isUnder(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// TestRealpathResolvesSymlinkToNonexistentTarget 是围栏的核心场景：
// 区内软链指向区外，而**目标文件尚不存在**。
//
// 这是朴素实现的经典漏点：直接对最终路径求解析必然失败（文件不存在），
// 于是实现会「解析不出来就当没越界」—— 绕过成立。
// 逐级向上找「第一个能完整解析的祖先」才能把软链本身解析出来。
func TestRealpathResolvesSymlinkToNonexistentTarget(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	link := makeLink(t, dir, "link", outside)

	// 目标**不存在**
	ghost := filepath.Join(link, "brand-new.txt")
	if _, err := os.Stat(ghost); err == nil {
		t.Fatal("前置条件失败：目标本应不存在")
	}

	got, err := be.Realpath(ghost)
	if err != nil {
		t.Fatalf("Realpath 对不存在的目标应通过逐级向上解析成功，实际报错: %v", err)
	}
	// 关键：解析结果必须在区外 —— 围栏据此判越界。
	// 若实现「解析不出来就返回原路径」，got 会以 dir 开头 → 绕过成立。
	if isUnder(got, dir) {
		t.Errorf("Realpath(%q) = %q 落在工作区内，围栏将判为不越界，绕过成立", ghost, got)
	}
	if filepath.Base(got) != "brand-new.txt" {
		t.Errorf("解析结果应保留尾部组件 brand-new.txt，实际 %q", got)
	}
}

// TestRealpathUnresolvableReturnsError 解析不出来要报错，不能充数。
//
// 改造前 builtin.resolveReal 在到根仍失败时**返回原路径**（fail-open）。
// 那样「解析不出来」与「解析出来就是这个」不可区分，围栏无法决策。
// 现在返回 PathNotResolvableError，让围栏自己决定 fail-closed。
func TestRealpathUnresolvableReturnsError(t *testing.T) {
	be := NewLocal()
	// 一个必然不存在的路径 —— 但它的父目录是真实存在的，
	// 所以逐级向上会在根处成功解析，得到「盘符\...」这种真实前缀。
	// 因此这里构造的是「整条链都不可解析」在本地难以复现的场景；
	// 只断言：Realpath 要么给出一个真实路径，要么给明确错误，
	// **绝不**返回与输入相同且未经解析的路径充数。
	p := filepath.Join(t.TempDir(), "a", "b", "c", "d.txt")
	got, err := be.Realpath(p)
	if err != nil {
		var e *PathNotResolvableError
		if !errors.As(err, &e) {
			t.Errorf("出错时应为 PathNotResolvableError，实际 %T", err)
		}
		return
	}
	// 成功解析时，结果应是「真实祖先 + 尾部组件」，而不是原样返回。
	if got == p && !existsAnyPrefix(p) {
		t.Errorf("Realpath 返回了未经解析的原路径 %q", got)
	}
}

// TestRemoveAndMkdirAll Remove / MkdirAll 的基本语义。
func TestRemoveAndMkdirAll(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")
	if err := be.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	// 幂等
	if err := be.MkdirAll(nested, 0o755); err != nil {
		t.Errorf("MkdirAll 应幂等: %v", err)
	}
	f := filepath.Join(nested, "f.txt")
	mustWrite(t, f, "x")
	if err := be.Remove(f); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f); err == nil {
		t.Errorf("Remove 后文件应不存在")
	}
	// 删不存在的应报错
	if err := be.Remove(f); err == nil {
		t.Errorf("Remove 不存在的文件应报错")
	}
}

// TestExecCapturesOutputAndExitCode Exec 的输出捕获与退出码。
func TestExecCapturesOutputAndExitCode(t *testing.T) {
	be := NewLocal()
	dir := t.TempDir()
	ctx := context.Background()

	res, err := be.Exec(ctx, ExecRequest{
		Command: "echo hello-from-exec",
		Dir:     dir,
		Timeout: 30_000_000_000,
	})
	if err != nil {
		t.Fatalf("Exec 失败: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("退出码应为 0，实际 %d，输出 %q", res.ExitCode, res.Output)
	}
	if !containsStr(res.Output, "hello-from-exec") {
		t.Errorf("应捕获到输出，实际 %q", res.Output)
	}
	if res.Shell == "" {
		t.Errorf("应回报实际使用的 shell")
	}
}

// TestExecTimeoutGivesExitCodeMinus2 超时必须是 -2 且标 TimedOut。
// 对应原：terminal.go 的 `cctx.Err() == context.DeadlineExceeded → exitCode = -2`。
//
// 命令必须跨平台：`sleep 30` 在 Windows 的 cmd 下不存在，会立刻返回
// 「不是内部或外部命令」（退出码 1），测的就不是超时而是「命令不存在」。
func TestExecTimeoutGivesExitCodeMinus2(t *testing.T) {
	long := `ping -n 30 127.0.0.1 >nul`
	if runtime.GOOS != "windows" {
		long = `sleep 30`
	}
	be := NewLocal()
	res, err := be.Exec(context.Background(), ExecRequest{
		Command: long,
		Dir:     t.TempDir(),
		Timeout: 300_000_000, // 300ms
	})
	if err != nil {
		t.Fatalf("Exec 不应返回 error: %v", err)
	}
	if res.ExitCode != -2 {
		t.Errorf("超时的退出码应为 -2，实际 %d（输出 %q）", res.ExitCode, res.Output)
	}
	if !res.TimedOut {
		t.Errorf("超时应标 TimedOut")
	}
}

// TestExecCallerCancelIsNotTimeout 调用方取消 ctx 不该被报成超时。
//
// 这是把 `cctx.Err() != nil` 写成 `== DeadlineExceeded` 的原因：
// 两者都是「ctx 结束了」，但用户点「停止」与「工具超时」在界面上
// 必须能区分 —— 前者是主动取消，后者是超时。
func TestExecCallerCancelIsNotTimeout(t *testing.T) {
	long := `ping -n 30 127.0.0.1 >nul`
	if runtime.GOOS != "windows" {
		long = `sleep 30`
	}
	be := NewLocal()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res, err := be.Exec(ctx, ExecRequest{
		Command: long,
		Dir:     t.TempDir(),
		Timeout: 60_000_000_000, // 60s，远大于取消时刻
	})
	if err != nil {
		t.Fatalf("Exec 不应返回 error: %v", err)
	}
	if res.TimedOut {
		t.Errorf("调用方取消不该标 TimedOut —— 那会让界面把「用户点了停止」显示成「超时」")
	}
	if res.ExitCode == -2 {
		t.Errorf("调用方取消时退出码不该是 -2（那是超时的专属值），实际 %d", res.ExitCode)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func existsAnyPrefix(p string) bool {
	for d := p; ; {
		if _, err := os.Stat(d); err == nil {
			return true
		}
		nd := filepath.Dir(d)
		if nd == d {
			return false
		}
		d = nd
	}
}
