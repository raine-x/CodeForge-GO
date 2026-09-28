//go:build windows

package builtin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/tools"
)

// Windows 分支的进程组测试。
//
// 与 Unix 用例验的是同一件事的两面：
//   - Unix    靠 Setpgid（在 exec 前生效，子进程天然继承）+ 向进程组发信号
//   - Windows 靠 Job Object（KILL_ON_JOB_CLOSE）+ taskkill /T 补漏
//
// 但 **WaitDelay 两边都必须配**，且它验的不是「进程有没有被杀」，
// 而是「cmd.Wait 会不会永久阻塞在管道 EOF 上」—— 这一点与平台无关。
//
// cmd.exe /c 同样会派生子进程（PowerShell、go build、npm…），
// 只杀 cmd.exe 本身不杀子孙，所以进程组管理同样必要。

// TestWindowsNoChildSurvivesTimeout 核心用例：超时后派生进程必须全部消失。
func TestWindowsNoChildSurvivesTimeout(t *testing.T) {
	dir := t.TempDir()
	probe := copyPingAsProbe(t, dir)

	fs := NewFS(dir)
	tool := NewTerminalTool(fs)
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	res, _ := tool.Execute(ctx, mustArgs(t, map[string]any{
		// start /b 后台派生探针；前台 ping 撑到超时
		"command":     fmt.Sprintf("start /b %s -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1", probe),
		"timeout_sec": 1,
	}))
	if res == nil {
		t.Fatal("不应返回 nil")
	}

	// 给 OS 一点回收时间
	time.Sleep(500 * time.Millisecond)
	if n := countProbeProcesses(probe); n > 0 {
		t.Errorf("超时后仍有 %d 个探针进程存活 —— 子进程泄漏，会持续吃 CPU/内存", n)
	}
}

// TestWindowsWaitDelayBoundsWait 单独把 WaitDelay 拎出来测。
//
// 这个用例是 1.2 修复的直接证据：改造前 cmd.Wait() 阻塞在管道 EOF 上，
// 工具 20 秒都不返回（实测 59 秒），连退出码判定都失效。
func TestWindowsWaitDelayBoundsWait(t *testing.T) {
	dir := t.TempDir()
	probe := copyPingAsProbe(t, dir)

	fs := NewFS(dir)
	tool := NewTerminalTool(fs)
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		tool.Execute(ctx, mustArgs(t, map[string]any{
			"command":     fmt.Sprintf("start /b %s -n 60 127.0.0.1 >nul & ping -n 60 127.0.0.1", probe),
			"timeout_sec": 1,
		}))
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("超时后 20 秒仍未返回 —— cmd.Wait() 阻塞在管道 EOF，WaitDelay 没生效")
	}

	time.Sleep(500 * time.Millisecond)
	if n := countProbeProcesses(probe); n > 0 {
		t.Errorf("仍有 %d 个探针存活", n)
	}
}

// TestWindowsSuccessfulCommandUnaffected 回归：正常执行的命令不该被误杀。
func TestWindowsSuccessfulCommandUnaffected(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	tool := NewTerminalTool(fs)
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	res, _ := tool.Execute(ctx, mustArgs(t, map[string]any{
		"command":     "echo hello-cf",
		"timeout_sec": 10,
	}))
	if res == nil || !res.Success {
		t.Fatalf("正常命令不该失败: %+v", res)
	}
	md, _ := res.Data.(map[string]any)
	if md == nil {
		t.Fatalf("Data 结构变了: %T", res.Data)
	}
	if out, _ := md["output"].(string); !strings.Contains(out, "hello-cf") {
		t.Errorf("输出不对: %q", md["output"])
	}
	if code, _ := md["exit_code"].(int); code != 0 {
		t.Errorf("退出码应为 0，实为 %v", md["exit_code"])
	}
}

// copyPingAsProbe 复制一份 ping.exe 改成本用例独有的名字。
//
// 不能直接用 ping 做断言 —— 同名进程可能是别的测试或用户程序起的，
// 断言会变成「间歇性失败」。改名后按名计数才是确定的。
func copyPingAsProbe(t *testing.T, dir string) string {
	t.Helper()
	src := filepath.Join(os.Getenv("SystemRoot"), "System32", "ping.exe")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("读不到 ping.exe，跳过: %v", err)
	}
	dst := filepath.Join(dir, fmt.Sprintf("cfprobe_%d_%d.exe", os.Getpid(), time.Now().UnixNano()))
	if err := os.WriteFile(dst, raw, 0o755); err != nil {
		t.Fatalf("写探针失败: %v", err)
	}
	return dst
}

// countProbeProcesses 按进程名数探针。
func countProbeProcesses(probePath string) int {
	name := strings.TrimSuffix(filepath.Base(probePath), ".exe")
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+name+".exe", "/NH").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), strings.ToLower(name)) {
			n++
		}
	}
	return n
}
