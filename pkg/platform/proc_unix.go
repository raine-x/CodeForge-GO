//go:build !windows

package platform

import (
	"os/exec"
	"syscall"
)

// StartGrouped 启动命令，并让它成为新进程组的组长。
//
// 两件必须配对做的事在这里一次做完：
//
//  1. Setpgid 必须在 Start **之前**设，否则子进程已经跑起来了。
//  2. cmd.Cancel 覆盖掉 exec.CommandContext 的默认实现（只 Process.Kill），
//     改成向**整个进程组**发信号。必须在 Start 之前设 —— os/exec 的
//     watchCtx goroutine 在 Start 里启动，Cancel 之后改会有数据竞争。
//
// 之所以把 Start 也包进来：进程组设置必须在 Start 之前、而 Windows 的
// 入组必须在 Start 之后，两边时序不同，调用方无法自己统一。
func StartGrouped(cmd *exec.Cmd) (*Group, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true

	cmd.Cancel = func() error {
		// 负 pgid = 发给组内所有进程。
		// 只 Process.Kill() 的话，杀掉的只是 shell 本身 ——
		// `bash -c "go build ./..."` 里的 go 编译器及其 worker 会活下来继续吃 CPU。
		if cmd.Process == nil {
			return nil
		}
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
				return nil
			}
		}
		// 兜底：拿不到 pgid 就退回只杀直接子进程（旧行为）
		return cmd.Process.Kill()
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Group{cmd: cmd}, nil
}

// CloseGroup 在 Unix 下是空操作（没有需要释放的句柄）。
func CloseGroup(g *Group) {}
