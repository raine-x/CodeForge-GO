package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 指向目录的符号链接必须被归入**目录**列表。
//
// 这是文件选择器的实打实错判：DirEntry.IsDir() 走 lstat 语义，报告的是
// 链接自身的类型，于是「软链 → 目录」IsDir() 为 false，被归进文件列表 ——
// 用户能选中它、能点「选择此文件」，拿回来一个目录路径。
func TestListTreeSymlinkToDirIsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows 建符号链接要管理员或开发者模式，没有就跳过而不是失败。
		if err := os.Symlink(t.TempDir(), filepath.Join(t.TempDir(), "probe")); err != nil {
			t.Skipf("本机无法创建符号链接（Windows 需开发者模式）: %v", err)
		}
	}

	root := t.TempDir()
	realDir := filepath.Join(root, "real_dir")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 真实文件，用来确认修复没有把真文件也判成目录
	if err := os.WriteFile(filepath.Join(root, "real_file.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "link_to_dir")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("无法创建指向目录的符号链接: %v", err)
	}

	items := listTree(root, 1)
	byName := map[string]treeNode{}
	for _, it := range items {
		byName[it.Name] = it
	}

	ln, ok := byName["link_to_dir"]
	if !ok {
		t.Fatalf("列表里应包含 link_to_dir，实际: %+v", names(items))
	}
	if !ln.IsDir {
		t.Errorf("指向目录的符号链接应判为目录（is_dir=true），实际 false —— " +
			"它会被归进文件列表、能被选中，确认后返回目录路径")
	}

	fn, ok := byName["real_file.txt"]
	if !ok {
		t.Fatalf("列表里应包含 real_file.txt")
	}
	if fn.IsDir {
		t.Errorf("真实文件不应被判为目录")
	}
	if _, ok := byName["real_dir"]; !ok {
		t.Errorf("真实目录应出现在列表里（修复没把它们过滤掉）")
	}
}

// 悬空符号链接（目标已删除）不能 panic，也不能被判成目录。
func TestListTreeDanglingSymlink(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "dangling")
	if err := os.Symlink(filepath.Join(root, "nope"), link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	// 关键：不 panic。Stat 失败时保留 IsDir() 的原值（false），
	// 让它以「文件」出现 —— 选不选得动由用户判断，而不是服务崩溃。
	items := listTree(root, 1)
	for _, it := range items {
		if it.Name == "dangling" && it.IsDir {
			t.Errorf("悬空符号链接不应被判为目录")
		}
	}
}

func names(items []treeNode) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}
