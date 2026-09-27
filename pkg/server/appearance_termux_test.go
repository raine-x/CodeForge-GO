package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Termux 上「装了 termux-tools 也换不了背景图」的回归防线。
//
// 故障有两层，都不在这条文件里而在实现里，这里把两条都钉住：
//  1. 探测只看进程 PATH 里的 termux-open-url。CodeForge 若不是从 Termux 会话
//     拉起（Termux:API、桌面快捷方式、某些启动器），PATH 里没有 $PREFIX/bin，
//     于是明明装过也被判成「未安装」，启动反复弹安装建议。
//  2. termux-storage-get 一失败就把错误写成「需要 termux-tools」，真实原因
//     （存储未授权 / SAF 要前台）全被掩盖，用户被这句话带偏。

// TestLookTermuxPathFindsInPrefixBin 命令不在 PATH、只在 $PREFIX/bin 时也要能找到。
//
// 做法是造一个假的 $PREFIX/bin 目录并清空 PATH：Windows 上没有 $PREFIX/bin 这个
// 约定目录，但 lookTermuxPath 只是 Join + LookPath，与平台无关。
func TestLookTermuxPathFindsInPrefixBin(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(bin, "termux-open-url")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", prefix)
	t.Setenv("PATH", t.TempDir()) // 清空，确保只能靠 $PREFIX/bin 命中

	got, err := lookTermuxPath("termux-open-url")
	if err != nil {
		t.Fatalf("命令只在 $PREFIX/bin 时也应找得到，实际报错: %v", err)
	}
	if filepath.Base(got) != "termux-open-url" {
		t.Errorf("应返回命令路径本身，实际 %q", got)
	}
	if _, err := lookTermuxPath("termux-not-installed"); err == nil {
		t.Error("不存在的命令必须报错，不能凭空判定已安装")
	}
}

// TestTermuxMissingCommandsReportsPerCommand 缺哪个报哪个，不是一句笼统的「未安装」。
func TestTermuxMissingCommandsReportsPerCommand(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// 只装一个：termux-open-url（自动开浏览器代表性命令）。
	if err := os.WriteFile(filepath.Join(bin, "termux-open-url"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PREFIX", prefix)
	t.Setenv("PATH", t.TempDir())

	missing := termuxMissingCommands()
	if len(missing) != 2 {
		t.Fatalf("应逐项报出缺的两个命令，实际 %v", missing)
	}
	joined := strings.Join(missing, ",")
	for _, want := range []string{"termux-storage-get", "termux-setup-storage"} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺失列表应含 %q，实际 %v", want, missing)
		}
	}
	if termuxToolsReady() {
		t.Error("只装一个命令时不应判定为「termux-tools 已安装」")
	}

	hint := termuxToolsHint(os.ErrNotExist, nil)
	if !strings.Contains(hint, "termux-tools") || !strings.Contains(hint, "pkg install termux-tools") {
		t.Errorf("缺命令时的提示应给出安装办法，实际 %q", hint)
	}
}

// TestTermuxToolsHintDistinguishesInstalled 三个命令都在时，提示要讲「已安装但调用失败」，
// 并把 termux-setup-storage 这条真正的解法说出来 —— 而不是甩一句「需要 termux-tools」。
func TestTermuxToolsHintDistinguishesInstalled(t *testing.T) {
	prefix := t.TempDir()
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"termux-open-url", "termux-storage-get", "termux-setup-storage"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PREFIX", prefix)
	t.Setenv("PATH", t.TempDir())

	if !termuxToolsReady() {
		t.Fatal("三个命令都装了，应判定为已安装（$PREFIX/bin 命中即可）")
	}
	hint := termuxToolsHint(os.ErrPermission, []byte("java.lang.SecurityException"))
	if strings.Contains(hint, "需要 termux-tools") || strings.Contains(hint, "未安装") {
		t.Errorf("已安装时不该说「未安装/需要 termux-tools」，实际 %q", hint)
	}
	if !strings.Contains(hint, "termux-setup-storage") {
		t.Errorf("已安装时应给出真正的解法（授权存储），实际 %q", hint)
	}
	if !strings.Contains(hint, "SecurityException") {
		t.Errorf("应带上上游的真实报错，便于自证，实际 %q", hint)
	}
}

// TestBgPickDestIsAbsoluteUnderWorkspace DataDir 是相对值（".codeforge"），
// 直接拼出来靠进程 CWD 才落得下去；工作区切换 / 从别处启动时就会写错位置。
func TestBgPickDestIsAbsoluteUnderWorkspace(t *testing.T) {
	root := t.TempDir()
	deps := newTestDeps(t)
	deps.cfg.DataDir = ".codeforge"
	deps.agent.SetWorkDir(root)
	srv := deps.newServer()

	got, err := srv.bgPickDest()
	if err != nil {
		t.Fatalf("bgPickDest 失败: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("落盘路径必须是绝对路径，实际 %q", got)
	}
	want := filepath.Join(root, ".codeforge", "background_user")
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Errorf("落盘路径期望 %q，实际 %q", want, got)
	}
}

// TestBgPickDestRejectsEmptyDataDir DataDir 为空时 filepath.Join("", x) 会得到 "x"，
// 等于把图片写进进程的工作目录 —— 必须报错而不是照做（AGENTS.md 记过同类坑）。
func TestBgPickDestRejectsEmptyDataDir(t *testing.T) {
	deps := newTestDeps(t)
	deps.cfg.DataDir = ""
	srv := deps.newServer()
	if _, err := srv.bgPickDest(); err == nil {
		t.Fatal("data_dir 为空时必须报错，不能拼出相对路径落盘")
	}
}

// TestBgPickDestRejectsNoWorkspace 工作区为空时无法确定基准目录，同样要报错。
func TestBgPickDestRejectsNoWorkspace(t *testing.T) {
	deps := newTestDeps(t)
	deps.cfg.DataDir = ".codeforge"
	deps.agent.SetWorkDir("")
	srv := deps.newServer()
	if _, err := srv.bgPickDest(); err == nil {
		t.Fatal("未选工作区时必须报错，不能依赖进程 CWD 落盘")
	}
}

// TestTermuxStartDirFallsBackToHome 没授权手机存储时不能把用户送进一个空列表：
// ~/storage/shared 不可列出内容就退回 ~。
func TestTermuxStartDirFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 上 os.UserHomeDir 读这个

	got := termuxStartDir()
	if got == "" {
		t.Fatal("起始目录不应为空")
	}
	if _, err := os.Stat(got); err != nil {
		t.Errorf("起始目录必须真实存在，否则内置选择器打开就是空列表：%q（%v）", got, err)
	}
	if got != home && !strings.HasPrefix(got, home) {
		t.Errorf("未授权时起始目录应落在用户主目录下，实际 %q，home=%q", got, home)
	}
}
