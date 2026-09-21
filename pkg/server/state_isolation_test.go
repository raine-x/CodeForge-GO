// 测试隔离：把用户级运行状态文件重定向到临时目录。
//
// state.yaml 与 data.db 同属「用户级、跨工作区共享」，而本包的用例密集地触发
// 设置保存（权限模式、外观、子智能体、模型切换）。不隔离的话，跑一次测试就会
// 改掉真实用户机器上的运行状态 —— 表现是「我上次设的背景/模型怎么自己变了」。
package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codeforge-server-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "测试隔离失败: "+err.Error())
		os.Exit(1)
	}
	if err := os.Setenv("CODEFORGE_STATE", filepath.Join(dir, "state.yaml")); err != nil {
		fmt.Fprintln(os.Stderr, "测试隔离失败: "+err.Error())
		os.Exit(1)
	}
	code := m.Run()
	_ = os.Unsetenv("CODEFORGE_STATE")
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
