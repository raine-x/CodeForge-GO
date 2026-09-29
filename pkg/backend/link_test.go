package backend

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// makeLink 在 dir 下建一个指向 target 的「目录链接」，返回链接路径。
//
// 为什么需要这层封装：Windows 上 os.Symlink 对**目录**需要 SeCreateSymbolicLink
// 权限（管理员或开发者模式），普通用户直接拿到 "此操作需要管理员权限"。
// 结果是围栏最关键的几个用例（区内软链指向区外）在 Windows CI 上
// 全部 t.Skip —— 而那正是最该跑的地方。
//
// 兜底方案是**目录联接**（junction，`mklink /J`）：
//   - 不需要特权；
//   - 对路径解析（EvalSymlinks）的效果与符号链接等价；
//   - 正是 3.2 要防的那种「区内一个链接指向区外」的场景。
//
// 两者都失败时才跳过，并说明原因 —— 不用「跳过」掩盖「跑不到」。
func makeLink(t *testing.T, dir, name, target string) string {
	t.Helper()
	link := dir + string(os.PathSeparator) + name

	symErr := os.Symlink(target, link)
	if symErr == nil {
		return link
	}
	if runtime.GOOS == "windows" {
		// 符号链接不可用 → 试目录联接。
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err == nil {
			return link
		}
		t.Skipf("既不能建符号链接也不能建目录联接，跳过。"+
			"symlink 错误：%v；mklink 输出：%s", symErr, out)
	}
	t.Skipf("建符号链接失败，跳过: %v", symErr)
	return ""
}
