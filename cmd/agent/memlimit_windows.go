//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// memoryStatusEx 对应 Win32 的 MEMORYSTATUSEX。
//
// 字段顺序与类型严格照 MSDN：两个 DWORD 之后是 7 个 SIZE_T（64 位下即 uint64），
// 因此自然对齐下总大小 64 字节，与 API 期望一致。**不可改字段顺序或类型**，
// Length 字段由 API 校验，不匹配会直接返回 FALSE。
type memoryStatusEx struct {
	Length                   uint32
	MemoryLoad               uint32
	TotalPhys                uint64
	AvailablePhys            uint64
	TotalPageFile            uint64
	AvailablePageFile        uint64
	TotalVirtual             uint64
	AvailableVirtual         uint64
	AvailableExtendedVirtual uint64
}

var (
	kernel32                 = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

// totalPhysMem 返回物理内存字节数；拿不到返回 0。
//
// 为什么 P/Invoke：Go 的 syscall 与 x/sys/windows 都**没有**绑定
// GlobalMemoryStatusEx，想用它只能自己声明。与其为此引一个依赖，
// 不如直接调 —— 这是一处稳定的 Win32 API，不会变。
//
// 用 syscall.NewLazyDLL 而非 NewLazySystemDLL：标准库只提供前者
// （后者在 x/sys 里，而该包同样没绑这个 API）。对 kernel32.dll 这类
// **常驻系统 DLL** 而言，搜索路径问题不存在 —— 它在进程启动时必然已加载。
//
// 注意它只报**物理内存**，与 cgroup / Job Object 的内存配额不是一回事：
// 容器里这个值会偏大，那种场景用 CODEFORGE_GOMEMLIMIT 显式覆盖更准
// （见 applyMemoryLimit）。
func totalPhysMem() uint64 {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	r, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if r == 0 {
		return 0
	}
	return ms.TotalPhys
}
