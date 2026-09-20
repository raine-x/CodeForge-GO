package main

import (
	"os"
	"path/filepath"
	"testing"
)

// resolveConfigDirAt 的回归测试。
//
// 背景：用户报「图片的设置以及状态下次启动就没了」。排查发现是启动时 CWD 不对
// （直接运行 bin/ 下的可执行文件）→ -config 的默认值 "config" 解析到 bin/config
// → 读不到 config/local.yaml → 外观/模型/密钥全部落回内置默认，但程序照常启动。
//
// 这里锁住「可执行文件旁」的回退：只要 <exeDir>/../<dir>/default.yaml 存在就改用它，
// 让直接运行 bin/ 下的可执行文件也能读到项目根的配置。
func TestResolveConfigDirAt(t *testing.T) {
	// 造一个仓库布局：<tmp>/bin/（exe 所在） + <tmp>/config/default.yaml
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "config", "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 用默认值 "config"（与真实场景一致）。测试 CWD 是本包目录，下面没有 config/，
	// 所以等价于「CWD 不对」，且不必 chdir（chdir 会影响整个进程）。
	const relName = "config"

	t.Run("CWD 下找不到时回退到可执行文件旁", func(t *testing.T) {
		got := resolveConfigDirAt(relName, binDir)
		want := filepath.Join(binDir, "..", relName)
		if filepath.Clean(got) != filepath.Clean(want) {
			t.Fatalf("应回退到 %q，实际 %q", want, got)
		}
		if _, err := os.Stat(filepath.Join(got, "default.yaml")); err != nil {
			t.Fatalf("回退目标下应有 default.yaml: %v", err)
		}
	})

	t.Run("可执行文件旁也没有时保持原样（交给告警，不猜）", func(t *testing.T) {
		// ⚠️ exeDir 要选「父目录里没有 config」的位置：
		// 回退是 <exeDir>/../<dir>，若 exeDir 直接用 <tmp>/elsewhere，
		// 父目录仍是 <tmp>，而 <tmp>/config 确实存在 → 会（正确地）回退，断言就假失败。
		got := resolveConfigDirAt(relName, filepath.Join(tmp, "bin", "deep"))
		if got != relName {
			t.Fatalf("应原样返回 %q，实际 %q", relName, got)
		}
	})

	t.Run("exeDir 为空时保持原样", func(t *testing.T) {
		if got := resolveConfigDirAt(relName, ""); got != relName {
			t.Fatalf("应原样返回 %q，实际 %q", relName, got)
		}
	})

	t.Run("绝对路径找不到时绝不改判", func(t *testing.T) {
		abs := filepath.Join(tmp, "nope-abs")
		if got := resolveConfigDirAt(abs, binDir); got != abs {
			t.Fatalf("绝对路径应原样返回 %q，实际 %q", abs, got)
		}
	})

	t.Run("CWD 下能找到时行为完全不变（不回退）", func(t *testing.T) {
		// 在测试 CWD 下真造一个目录，验证「能用就不动」
		local := filepath.Join(".", "cf-test-local-cfg")
		if err := os.MkdirAll(local, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(local) })
		if err := os.WriteFile(filepath.Join(local, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := resolveConfigDirAt(local, binDir); got != local {
			t.Fatalf("CWD 下可用时应原样返回 %q，实际 %q", local, got)
		}
	})
}
