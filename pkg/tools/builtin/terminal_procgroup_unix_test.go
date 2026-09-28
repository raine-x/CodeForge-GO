//go:build !windows

package builtin

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"codeforge/pkg/tools"
)

// 这组测试针对「终端超时后子进程存活」。
//
// 改造前 pkg/tools/builtin/terminal.go:90 只用 exec.CommandContext，
// 没有 Setpgid、没有 Job Object、也没有 cmd.WaitDelay：
//
//	bash -c "go build ./..."   超时后 bash 被杀，go 编译器及其 worker 继续吃 CPU
//	bash -c "npm run dev"      超时后 Node 存活，端口不释放
//	bash -c "sleep 1000"       孤儿进程永久驻留
//
// 长会话累积下来，进程表与内存都会被拖垮，而用户完全无感。
//
// 注意：本文件是 //go:build !windows。Windows 分支走 Job Object，
// 同样需要 cmd.WaitDelay，见 terminal_windows.go 的对应测试。

// TestTerminalCommandIsKilled 无论是否超时，命令派生的进程都不该活下来。
func TestTerminalCommandIsKilled(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	tool := NewTerminalTool(fs)
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	// 后台派生一个名字独特的进程，再让它等很久 —— 超时后 shell 被杀，
	// 若没做进程组管理，这个 sleep 会活下来。
	marker := "cf_procgroup_probe"
	script := "sleep 47 & echo " + marker + "; sleep 47"

	res, _ := tool.Execute(ctx, mustArgs(t, map[string]any{
		"command":     script,
		"timeout_sec": 1, // 1 秒后超时，远小于 sleep 47
	}))
	if res == nil {
		t.Fatal("不应返回 nil")
	}
	if !res.Success {
		t.Fatalf("超时是预期路径，不该报失败: %+v", res)
	}
	md, _ := res.Data.(map[string]any)
	if md == nil {
		t.Fatalf("Data 结构变了: %T", res.Data)
	}
	if code, _ := md["exit_code"].(int); code != -2 {
		t.Errorf("超时退出码应为 -2，实为 %v", md["exit_code"])
	}

	// 给 OS 一点时间回收
	time.Sleep(500 * time.Millisecond)
	if n := countProbeProcesses(marker); n > 0 {
		t.Errorf("超时后仍有 %d 个子进程存活 —— 未做进程组管理，会持续泄漏", n)
	}
}

// TestTerminalWaitDelayBoundsWait 验证「杀进程组后 cmd.Wait 仍会返回」。
//
// 这是必须与进程组**成对**的第二个修复：即使杀了整个进程组，
// 孙进程持有的 stdout/stderr 管道写端不会立刻关闭，而 cmd.Wait() 的语义是
// 「等进程退出 **且** 管道读到 EOF」。所以只要有一个孙进程攥着管道，
// cmd.Run() 就永久阻塞 —— 等于把「进程泄漏」换成「goroutine 泄漏 + 超时语义失效」，
// 后者更难查。
//
// 用例：命令超时后必然有子进程被杀，若 WaitDelay 缺失，工具会一直不返回。
func TestTerminalWaitDelayBoundsWait(t *testing.T) {
	dir := t.TempDir()
	fs := NewFS(dir)
	tool := NewTerminalTool(fs)
	ctx := tools.WithSession(context.Background(), tools.SessionScope{SessionID: "s1"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		// 后台派生 + 长前台：超时后前后台都被杀，管道写端随之关闭
		tool.Execute(ctx, mustArgs(t, map[string]any{
			"command":     "sleep 47 & sleep 47",
			"timeout_sec": 1,
		}))
	}()

	select {
	case <-done:
		// 正常
	case <-time.After(15 * time.Second):
		t.Fatal("超时后 15 秒仍未返回 —— cmd.Wait() 阻塞在管道 EOF，WaitDelay 没生效")
	}
}

// countProbeProcesses 数还活着的探针进程。
func countProbeProcesses(marker string) int {
	out, err := exec.Command("bash", "-c",
		"ps -eo args= | grep -F '"+marker+"' | grep -v grep | wc -l").Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range strings.TrimSpace(string(out)) {
		if c >= '0' && c <= '9' {
			n = n*10 + int(c-'0')
		}
	}
	return n
}
