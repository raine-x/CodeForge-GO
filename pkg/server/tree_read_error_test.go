package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// picker 浏览一个「路径能 stat、但内容读不出来」的目录时，必须把原因回报给前端，
// 不能与「这个目录真的是空的」混为一谈。
//
// 这条对应安卓上「弹出来一个空挂载」：Termux 未执行 termux-setup-storage 时，
// ~/storage 软链指向的 /storage/emulated/0 在 Android 11+ 的分区存储下
// ReadDir 直接失败。listTree 原先把 err 吞掉返回 nil，handleTree 于是回
// 200 + 空 items —— 前端只能渲染「无子目录」，用户看到的是一个
// **看起来正常、实则什么都读不到**的挂载点，唯一线索（权限）被丢掉了。
//
// 用例拿「路径是个普通文件」构造失败：ReadDir 对文件返回 ENOTDIR，
// 与权限失败走的是同一条错误通路，但不依赖 uid —— 本机是 root 时
// 000 权限构造不出失败（那样用例会静默跳过，等于没测）。
func TestListTreeReportsReadError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := listTree(file, 1)
	if err == nil {
		t.Fatalf("对文件做 ReadDir 应报错，实际 nil（items=%d）—— "+
			"错误被吞掉后前端只能显示「无子目录」", len(items))
	}
	if len(items) != 0 {
		t.Errorf("读取失败时不应编造条目，实际 %d 条", len(items))
	}
}

// 「真的空」必须与「读不出」区分开：空目录的 err 为 nil。
// 否则修复会把「空」也报成错误，用户以为坏了。
func TestListTreeEmptyDirIsNotAnError(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	items, err := listTree(empty, 1)
	if err != nil {
		t.Fatalf("真正的空目录不应被报成读取失败: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("空目录应返回 0 条，实际 %d", len(items))
	}
}

// 子目录读不出来时**不能让整个列表失败**：顶层可读就够了。
// 典型场景是 Android/data —— 顶层列得出来，逐个进去全部 EACCES。
func TestListTreeUnreadableChildDoesNotFailParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 用悬空软链冒充「stat 失败」：它会出现在列表里但进不去。
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "broken")); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	items, err := listTree(root, 2)
	if err != nil {
		t.Fatalf("顶层可读就不该报错（子层失败只跳过该子树）: %v", err)
	}
	if len(items) != 2 {
		t.Errorf("应列出 2 项，实际 %d：%v", len(items), names(items))
	}
}

// 安卓 Termux 上读手机存储失败时，文案必须指向可执行的下一步
// （termux-setup-storage），而不是把 errno 原样抛给用户。
func TestDescribeTreeReadErrorHintsTermuxStorage(t *testing.T) {
	perm := os.ErrPermission
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{"软链写法", "/data/data/com.termux/files/home/storage/shared", "termux-setup-storage"},
		{"真实路径", "/storage/emulated/0", "termux-setup-storage"},
		{"sdcard 别名", "/sdcard", "termux-setup-storage"},
	}
	for _, c := range cases {
		msg := describeTreeReadError(c.dir, &os.PathError{
			Op: "readdirent", Path: c.dir, Err: perm,
		})
		if msg == "" {
			t.Errorf("%s：describeTreeReadError 不应对失败返回空串", c.name)
			continue
		}
		if !strings.Contains(msg, c.want) {
			t.Errorf("%s：安卓上的读取失败应提示 %s，实际: %s", c.name, c.want, msg)
		}
	}
}

// 非手机存储路径不该被硬塞 termux-setup-storage（那是安卓专用命令，
// 在 Linux/Windows 上毫无意义），但仍要说清是权限问题。
func TestDescribeTreeReadErrorNonStoragePath(t *testing.T) {
	msg := describeTreeReadError("/var/lib/secret", &os.PathError{
		Op: "readdirent", Path: "/var/lib/secret", Err: os.ErrPermission,
	})
	if strings.Contains(msg, "termux-setup-storage") {
		t.Errorf("非手机存储路径不该提示 termux-setup-storage: %s", msg)
	}
	if !strings.Contains(msg, "无权限") {
		t.Errorf("权限失败应说明是无权限，实际: %s", msg)
	}
	// 未知错误也要有文案，不能退化成空串（前端会渲染成空白条）。
	if other := describeTreeReadError("/tmp/x", errors.New("boom")); other == "" {
		t.Error("未知错误也应有可读文案")
	}
}

// read_error 只在「读不出」时出现，可读的目录（含真的空的）一律不带。
//
// 这是前端区分「空目录」与「读不到目录」的唯一依据：一旦这里恒为非空，
// 空目录会被误报成错误；一旦恒为空，权限问题又会退回「无子目录」。
// 本机是 root 时构造不出权限失败，所以这里只钉住「不该出现时确实没有」。
func TestTreePickerNoReadErrorOnReadableDir(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()

	code, d := getTree(t, srv, "&picker=1")
	if code != http.StatusOK {
		t.Fatalf("picker 起始目录期望 200，实际 %d（%s）", code, d.Error)
	}
	if d.ReadError != "" {
		t.Errorf("可读的起始目录不应带 read_error: %s", d.ReadError)
	}
}
