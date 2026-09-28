// Package platform 里与「子进程生命周期」相关的部分。
//
// 单独成文件而不是塞进 terminal.go，是因为后者已经背了 Windows 盘符正则、
// MSYS 路径等一堆平台分支，再叠一层进程管理会彻底失控。已有的
// launcher.go / sys_windows.go / sys_linux.go 就是这个划分。
package platform

import (
	"os/exec"
	"time"
)

// ProcessWaitDelay 是 ctx 取消后、主动关闭 stdout/stderr 管道前给的收尾时间。
//
// 为什么必须有它：即使已经把整个进程组杀掉，**子进程持有的管道写端不会
// 立刻关闭**。而 cmd.Wait() 的语义是「等进程退出 **且** 管道读到 EOF」，
// 所以只要有一个孙进程还攥着管道，Wait 就永久阻塞。
//
// 也就是说：只做进程组、不配 WaitDelay，等于把「进程泄漏」换成
// 「goroutine 泄漏 + 终端超时语义失效」—— 后者更难查，因为工具根本不返回，
// 退出码判定（-2 = 超时）也一并失效。
//
// 取值 5 秒：太小会在正常的高输出量场景误关管道（正在写的输出被截断），
// 太大则让超时语义冗长。
const ProcessWaitDelay = 5 * time.Second

// Group 是一个**已启动、且已隔离到独立进程组**的子进程。
//
// 之所以要有这个类型而不是裸用 *exec.Cmd：进程组的建立与「入组」必须跨越
// cmd.Start() —— Unix 在 Start **之前**设 Setpgid，Windows 在 Start **之后**
// Assign 到 Job Object —— 而终止时还需要平台特有的句柄（Windows 的 job）。
//
// 把这三件事收在一个类型里，调用方就不必知道平台差异：terminal.go 只管
// 「StartGrouped → Wait → 超时就 KillGroup → 收尾 CloseGroup」。
type Group struct {
	cmd *exec.Cmd
	// priv 是平台私有状态：Unix 不用（留 nil），Windows 放 job 句柄。
	priv any
}
