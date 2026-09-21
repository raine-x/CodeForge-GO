// 测试隔离：运行状态与实例信息文件都重定向到临时目录。
//
// 本包负责写 ~/.codeforge/run.json（stop/restart 靠它定位实例）。
// 用例若不管它，测试进程会覆盖真实用户的实例信息 —— 之后 `codeforge stop`
// 就会去杀一个并不存在的 PID，或更糟：撞上真正的 PID 复用。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "codeforge-main-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "测试隔离失败: "+err.Error())
		os.Exit(1)
	}
	envs := map[string]string{
		"CODEFORGE_STATE": filepath.Join(dir, "state.yaml"),
		runEnvKey:         filepath.Join(dir, "run.json"),
	}
	for k, v := range envs {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, "测试隔离失败: "+err.Error())
			os.Exit(1)
		}
	}
	code := m.Run()
	for k := range envs {
		_ = os.Unsetenv(k)
	}
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
