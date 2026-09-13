package server

import (
	"runtime"
	"testing"
	"unicode/utf8"
)

// 回归：Windows 文件夹选择器必须把非 ASCII 路径原样取回。
//
// 背景（2026-09-13 实测踩到）：PowerShell 5.1 在 stdout 被重定向时用**控制台代码页**
// （中文 Windows 上是 gb2312/936）编码输出，`Write-Output $d.SelectedPath` 于是吐出
// GB2312 字节，Go 按 UTF-8 读就成了非法字符串 —— 用户选中含中文的目录，工作区路径会
// 静默损坏，还会一路写进会话的 workspace 键。
// 修法：脚本开头把 [Console]::OutputEncoding 设为 UTF-8。
//
// 本测试走 runPSDialog（目录/文件两个选择器共用的执行入口），把对话框换成固定路径，
// 保证以后有人「顺手清理」这句编码设置时会立刻红 —— 而且覆盖的是生产代码路径本身。
func TestPickFolderWindowsKeepsNonASCIIPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 的 PowerShell 输出编码问题")
	}
	want := `C:\Users\26536\Desktop\测试`
	got, err := runPSDialog(`Write-Output '` + want + `'`)
	if err != nil {
		t.Skipf("本机 PowerShell 不可用，跳过：%v", err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("取回的路径不是合法 UTF-8（控制台代码页污染）：%q", got)
	}
	if got != want {
		t.Fatalf("路径不一致：期望 %q，实际 %q", want, got)
	}
}

// 文件选择器共用同一入口，含中文的文件名同样必须原样取回。
func TestPickFileWindowsKeepsNonASCIIPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅 Windows 的 PowerShell 输出编码问题")
	}
	want := `C:\Users\26536\Desktop\测试\季度报告.md`
	got, err := runPSDialog(`Write-Output '` + want + `'`)
	if err != nil {
		t.Skipf("本机 PowerShell 不可用，跳过：%v", err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("取回的文件路径不是合法 UTF-8（控制台代码页污染）：%q", got)
	}
	if got != want {
		t.Fatalf("文件路径不一致：期望 %q，实际 %q", want, got)
	}
}
