package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// 内置目录选择器的 /api/tree?picker=1 模式。
//
// Termux 上「选择工作目录列表为空」的根因：内置选择器要浏览工作区**外**的目录
// （起始 ~ 或 ~/storage/shared），而 tree 对绝对路径一律要求落在工作区内 → 403 →
// 前端渲染成空列表。picker 模式放行工作区外绝对路径；普通模式维持原语义
// （相对路径基于工作区根、绝对路径必须区内），由 stage_file_test.go 锁定。

type treeItems struct {
	Error string `json:"error"`
	Path  string `json:"path"`
	Items []struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	} `json:"items"`
}

// getTree 请求 /api/tree（query 为已编码的追加参数），返回状态码与解码结果。
func getTree(t *testing.T, srv *Server, query string) (int, treeItems) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/tree?depth=1"+query, nil)
	rec := httptest.NewRecorder()
	srv.handleTree(rec, req)
	var d treeItems
	if rec.Code == http.StatusOK {
		if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, d
}

// qPath 对绝对路径做 URL query 编码（Windows 路径含反斜杠与盘符冒号）。
func qPath(p string) string { return "&path=" + url.QueryEscape(p) }

// TestTreePickerModeAllowsOutsideAbsolute picker=1 时浏览工作区外的绝对目录
// 必须放行（内置选择器挑工作区的前提），且能看到里面的子目录。
func TestTreePickerModeAllowsOutsideAbsolute(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()

	// 工作区外的目录（系统临时目录），里面放一个子目录
	outside := filepath.Join(t.TempDir(), "pickme")
	if err := os.MkdirAll(filepath.Join(outside, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	code, d := getTree(t, srv, "&picker=1"+qPath(outside))
	if code != http.StatusOK {
		t.Fatalf("picker 模式浏览区外绝对路径期望 200，实际 %d（%s）", code, d.Error)
	}
	if filepath.Clean(d.Path) != filepath.Clean(outside) {
		t.Errorf("返回 path = %q，期望 %q", d.Path, outside)
	}
	found := false
	for _, it := range d.Items {
		if it.IsDir && it.Name == "child" {
			found = true
		}
	}
	if !found {
		t.Errorf("应列出区外目录的子目录 child，实际 %v", d.Items)
	}
}

// TestTreePickerEmptyRootFallsHome picker=1 且未选工作区、未传 path 时
// 回落到用户主目录（否则「选工作区」入口永远无内容可浏览）。
func TestTreePickerEmptyRootFallsHome(t *testing.T) {
	deps := newTestDeps(t)
	deps.fsys.SetRoot("") // 模拟「未选择工作区」
	srv := deps.newServer()

	code, d := getTree(t, srv, "&picker=1")
	if code != http.StatusOK {
		t.Fatalf("picker 空根回落期望 200，实际 %d（%s）", code, d.Error)
	}
	if d.Path == "" {
		t.Error("picker 空根应回落到用户主目录，path 不应为空")
	}
}

// TestTreeOutsideAbsoluteStillForbidden 不带 picker 时区外绝对路径维持 403
// （普通文件树浏览的安全语义不变）。
func TestTreeOutsideAbsoluteStillForbidden(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()

	outside := t.TempDir() // 工作区外
	code, _ := getTree(t, srv, qPath(outside))
	if code != http.StatusForbidden {
		t.Fatalf("普通模式浏览区外绝对路径期望 403，实际 %d", code)
	}
}
