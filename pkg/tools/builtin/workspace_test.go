package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newGuardFS 造一个「工作区 + 区外目录」的测试夹具。
// 返回的 outside 里放着一个 secret.txt，用来验证内容不会被泄露。
func newGuardFS(t *testing.T) (fs *FS, root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "ws")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOPSECRET"), 0o644); err != nil {
		t.Fatalf("造区外测试文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("造区内测试文件失败: %v", err)
	}
	return NewFS(root), root, outside
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化参数失败: %v", err)
	}
	return b
}

// TestResolveCheckedAllowsInside 区内路径（相对 / 绝对 / 空）都不应被拒绝。
func TestResolveCheckedAllowsInside(t *testing.T) {
	fs, root, _ := newGuardFS(t)

	cases := []string{
		"inside.txt",
		filepath.Join("sub", "a.txt"),
		filepath.Join(root, "sub", "a.txt"),
		"", // 空路径 = 工作区根
	}
	for _, in := range cases {
		got, err := fs.ResolveChecked(in)
		if err != nil {
			t.Errorf("区内路径 %q 被误拒: %v", in, err)
			continue
		}
		if !within(root, got) {
			t.Errorf("区内路径 %q 解析到 %q，落在区外", in, got)
		}
	}
}

// TestResolveCheckedRejectsEscape 相对穿越与绝对路径越界都必须被拒绝。
func TestResolveCheckedRejectsEscape(t *testing.T) {
	fs, _, outside := newGuardFS(t)

	cases := map[string]string{
		"相对穿越":   filepath.Join("..", "outside", "secret.txt"),
		"多层穿越":   filepath.Join("..", "..", "outside", "secret.txt"),
		"绝对路径越界": filepath.Join(outside, "secret.txt"),
	}
	for name, in := range cases {
		_, err := fs.ResolveChecked(in)
		if err == nil {
			t.Errorf("%s: %q 应被拒绝，但放行了", name, in)
			continue
		}
		if !errors.Is(err, ErrOutsideWorkspace) {
			t.Errorf("%s: 错误类型不对: %v", name, err)
		}
	}
}

// TestResolveCheckedAllowOutsideSwitch 开关打开后不再拦截。
func TestResolveCheckedAllowOutsideSwitch(t *testing.T) {
	fs, _, outside := newGuardFS(t)
	target := filepath.Join(outside, "secret.txt")

	if _, err := fs.ResolveChecked(target); err == nil {
		t.Fatal("默认应拒绝越界路径")
	}
	fs.SetAllowOutside(true)
	if !fs.AllowOutside() {
		t.Fatal("SetAllowOutside(true) 未生效")
	}
	if _, err := fs.ResolveChecked(target); err != nil {
		t.Fatalf("放行开关打开后不应再拒绝: %v", err)
	}
}

// TestResolveCheckedRejectsSymlinkEscape 区内软链接指向区外时也要拦住。
func TestResolveCheckedRejectsSymlinkEscape(t *testing.T) {
	fs, root, outside := newGuardFS(t)

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("当前环境不支持创建软链接，跳过: %v", err)
	}
	if _, err := fs.ResolveChecked(filepath.Join("link", "secret.txt")); err == nil {
		t.Fatal("经由软链接指向区外的路径应被拒绝")
	}
}

// TestReadFileToolRejectsOutside 只读工具是自动放行的，因此这层防护尤其关键：
// 既要拒绝，也不能把区外内容带进结果里。
func TestReadFileToolRejectsOutside(t *testing.T) {
	fs, root, outside := newGuardFS(t)
	tool := NewReadFileTool(fs)

	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": filepath.Join(outside, "secret.txt"),
	}))
	if err != nil {
		t.Fatalf("Execute 不应返回 Go error: %v", err)
	}
	if res.Success {
		t.Fatal("读取区外文件应失败")
	}
	if strings.Contains(res.Error, "TOPSECRET") {
		t.Fatal("错误信息里泄露了区外文件内容")
	}
	if s, ok := res.Data.(string); ok && strings.Contains(s, "TOPSECRET") {
		t.Fatal("结果数据里泄露了区外文件内容")
	}

	res, err = tool.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": filepath.Join(root, "inside.txt"),
	}))
	if err != nil {
		t.Fatalf("Execute 不应返回 Go error: %v", err)
	}
	if !res.Success {
		t.Fatalf("读取区内文件应成功，实际失败: %s", res.Error)
	}
	if s, _ := res.Data.(string); s != "hello" {
		t.Fatalf("区内文件内容不对: %v", res.Data)
	}
}

// TestWriteFileToolRejectsOutside 写工具同样要拦，且不能真的把文件写出去。
func TestWriteFileToolRejectsOutside(t *testing.T) {
	fs, _, outside := newGuardFS(t)
	tool := NewWriteFileTool(fs)
	target := filepath.Join(outside, "pwned.txt")

	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]string{
		"path": target, "content": "pwned",
	}))
	if err != nil {
		t.Fatalf("Execute 不应返回 Go error: %v", err)
	}
	if res.Success {
		t.Fatal("写入区外文件应失败")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("区外文件被真的写出来了")
	}
}

// TestSearchToolRejectsOutside 检索起始目录越界也要拦。
func TestSearchToolRejectsOutside(t *testing.T) {
	fs, _, outside := newGuardFS(t)
	tool := NewSearchTool(fs)

	res, err := tool.Execute(context.Background(), mustJSON(t, map[string]string{
		"query": "TOPSECRET", "path": outside,
	}))
	if err != nil {
		t.Fatalf("Execute 不应返回 Go error: %v", err)
	}
	if res.Success {
		t.Fatal("检索区外目录应失败")
	}
}
