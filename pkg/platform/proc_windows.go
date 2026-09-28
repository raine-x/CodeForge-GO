//go:build windows

package platform

import (
	"context"
	"os/exec"
	"strconv"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winProc 承载 Windows 侧需要跨 Start 存活的状态。
type winProc struct {
	job windows.Handle
}

func (s *winProc) kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid

	// 第一步：taskkill /T —— 补 Assign 之前的竞态窗口。
	//
	// Windows 这边有一个 Unix 没有的固有缺陷：AssignProcessToJobObject 只能在
	// Start **之后**做（要子进程 pid），而 Start 返回时 cmd.exe 已经在跑了，
	// 它可能已经把 `start /b ping …` 派生的进程建出来了。Job 不会追溯纳入
	// 既有子进程，所以那些进程会逃出去。
	// 实测：只靠 Job Object 的话，一次超时能漏 6 个 ping。
	//
	// taskkill /T 按 PPID 遍历当前进程树，能兜住这个窗口。
	// Unix 不需要这一步 —— Setpgid 在 exec **之前**生效，子进程天然继承 pgid。
	killTreeByPID(pid)

	// 第二步：TerminateJobObject —— 兜住第一步快照之后新生的进程。
	if s.job != 0 {
		_ = windows.TerminateJobObject(s.job, 1)
		return nil
	}
	// 没入组时的兜底：只杀直接子进程（旧行为）
	return cmd.Process.Kill()
}

// killTreeByPID 调 taskkill /T /F 杀整棵进程树。
//
// 不在 PATH 上（精简版 Windows 容器镜像可能缺）就静默跳过 —— 上面的
// TerminateJobObject 仍是主力，这只是补漏。
func killTreeByPID(pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 隐藏窗口执行，否则每次超时会闪一个黑框
	_ = exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
}

// StartGrouped 启动命令并把它放进一个 Job Object。
//
// 为什么 Windows 必须用 Job Object：Windows 没有 Unix 那种进程组概念。
// CreateProcess 的 CREATE_NEW_PROCESS_GROUP 只影响 Ctrl+C 的传递行为，
// **杀不掉孙进程** —— `cmd /c "start /b ping …"` 派生的进程就是典型。
//
// 时序（这几步的先后是硬约束）：
//
//	CreateJobObject → 设 KILL_ON_JOB_CLOSE → 覆盖 cmd.Cancel → Start
//	→ AssignProcessToJobObject（必须 Start 之后，要子进程 pid）
//
// cmd.Cancel 必须在 Start 之前覆盖 —— os/exec 的 watchCtx goroutine 在
// Start 里启动，Start 后再改 Cancel 会数据竞争。
//
// 入组失败不中止命令：退回「只杀直接子进程」的旧行为，好过命令跑不起来。
func StartGrouped(cmd *exec.Cmd) (*Group, error) {
	st := &winProc{}
	cmd.Cancel = func() error { return st.kill(cmd) }

	job, jerr := createKillOnCloseJob()
	if jerr == nil {
		st.job = job
	}

	if err := cmd.Start(); err != nil {
		if st.job != 0 {
			_ = windows.CloseHandle(st.job)
			st.job = 0
		}
		return nil, err
	}
	if st.job != 0 {
		if aerr := windows.AssignProcessToJobObject(st.job, windows.Handle(cmd.Process.Pid)); aerr != nil {
			_ = windows.CloseHandle(st.job)
			st.job = 0
		}
	}
	return &Group{cmd: cmd, priv: st}, nil
}

// createKillOnCloseJob 建一个「句柄关闭即终止组内所有进程」的 Job Object。
func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// CloseGroup 关闭 job 句柄。JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 会在此刻
// 连带终止组内**所有**进程 —— 包括正常执行完毕后仍在后台驻留的孙进程。
// 这正是「终端工具应当是受控的」想要的效果。
func CloseGroup(g *Group) {
	if g == nil {
		return
	}
	if st, ok := g.priv.(*winProc); ok && st != nil && st.job != 0 {
		_ = windows.CloseHandle(st.job)
		st.job = 0
	}
}
