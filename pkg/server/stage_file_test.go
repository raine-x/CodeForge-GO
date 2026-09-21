package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestPathWithin 词法判断工作区边界：区内含子路径放行，区外/同级前缀目录拒绝。
func TestPathWithin(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	cases := []struct {
		path string
		want bool
	}{
		{root, true},
		{filepath.Join(root, "a.go"), true},
		{filepath.Join(root, "sub", "a.go"), true},
		{root + "2", false},                     // 同前缀不同目录必须拒绝
		{filepath.Join(root, "..", "x"), false}, // 穿越拒绝
	}
	for _, c := range cases {
		if got := pathWithin(root, c.path); got != c.want {
			t.Errorf("pathWithin(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestStageFileCopiesOutside 区外文件应被复制到 attachments/ 并返回相对路径；
// 重复 stage 同一文件时复用已有副本（reused=true）不再复制。
func TestStageFileCopiesOutside(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	// 区外源文件（工作区之外的 TempDir 兄弟目录）
	outside := t.TempDir()
	src := filepath.Join(outside, "外部分析.xlsx")
	if err := os.WriteFile(src, []byte("BINARY-DATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 第一次 stage：复制
	resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": src})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stage 区外文件失败: HTTP %d", resp.StatusCode)
	}
	var d1 struct {
		OK         bool   `json:"ok"`
		Inside     bool   `json:"inside"`
		StagedPath string `json:"staged_path"`
		Name       string `json:"name"`
		Reused     bool   `json:"reused"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d1); err != nil {
		t.Fatal(err)
	}
	if !d1.OK || d1.Inside || d1.StagedPath == "" || d1.Name != "外部分析.xlsx" {
		t.Fatalf("stage 返回异常: %+v", d1)
	}
	// 副本必须真的存在且内容一致
	dst := filepath.Join(deps.dir, filepath.FromSlash(d1.StagedPath))
	data, err := os.ReadFile(dst)
	if err != nil || string(data) != "BINARY-DATA" {
		t.Fatalf("副本缺失或内容不符: %v", err)
	}

	// 第二次 stage 同一文件：复用
	resp2 := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": src})
	var d2 struct {
		Reused bool `json:"reused"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&d2); err != nil {
		t.Fatal(err)
	}
	if !d2.Reused {
		t.Fatal("重复 stage 相同内容文件应复用副本（reused=true）")
	}
}

// TestStageFileResolvesRelativeToWorkspace 相对路径的解析基准必须是**工作区根**
// （与文件工具 FS.Resolve 的约定一致：相对路径基于工作区根），不能落到进程 CWD。
//
// 否则用户/模型写 `@sub/a.go` 时，服务端会去「进程启动目录」找同名文件：
// 找不到 → 报「文件不存在」这种莫名其妙的错；**恰好找到 → 把另一个文件静默拷进
// attachments/**，模型读到的完全是别的东西。
func TestStageFileResolvesRelativeToWorkspace(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	sub := filepath.Join(deps.dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sub, "a.go")
	if err := os.WriteFile(target, []byte("package a"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": "sub/a.go"})
	var d struct {
		OK     bool   `json:"ok"`
		Inside bool   `json:"inside"`
		Path   string `json:"staged_path"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !d.OK || !d.Inside {
		t.Fatalf("工作区内的相对路径应判定为「区内直通」，实际: %+v（HTTP %d）", d, resp.StatusCode)
	}
	if !filepath.IsAbs(d.Path) {
		t.Fatalf("直通应返回可用的绝对路径（模型要能直接用），实际 %q", d.Path)
	}
	if filepath.Clean(d.Path) != filepath.Clean(target) {
		t.Errorf("应解析到工作区内的 sub/a.go，实际 %q", d.Path)
	}
	if _, err := os.Stat(filepath.Join(deps.dir, "attachments")); err == nil {
		t.Error("区内文件不该被复制到 attachments/")
	}
}

// TestTreeRelativePathIsWorkspaceRelative 锁定 handleTree 的相对路径语义 =
// **相对工作区根**（且 `..` 被夹在根内，出不去）。
//
// 这条与 /api/workspace 的「必须给绝对路径」互为对照 —— 两个接口的相对路径基准
// 必须各自明确，历史上正是因为基准不统一才出过「同一个词在不同接口里指不同目录」的问题。
func TestTreeRelativePathIsWorkspaceRelative(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	sub := filepath.Join(deps.dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	type treeResp struct {
		Root  string `json:"root"`
		Path  string `json:"path"`
		Items []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
		} `json:"items"`
	}
	fetchTree := func(q string) treeResp {
		t.Helper()
		resp, err := client.Get(ts.URL + "/api/tree?depth=1&path=" + q)
		if err != nil {
			t.Fatalf("GET /api/tree?path=%s 失败: %v", q, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/tree?path=%s 期望 200，实际 %d", q, resp.StatusCode)
		}
		var d treeResp
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	// 相对路径 "sub" → <工作区>/sub
	if got := fetchTree("sub").Path; filepath.Clean(got) != filepath.Clean(sub) {
		t.Errorf("相对路径应基于工作区根解析：期望 %q，实际 %q", sub, got)
	}
	// `..` 穿越被夹在根内：不管写多少层，最多回到工作区根
	if got := fetchTree("..%2F..%2F..").Path; filepath.Clean(got) != filepath.Clean(deps.dir) {
		t.Errorf("`..` 应被夹在工作区根内：期望 %q，实际 %q", deps.dir, got)
	}
	// 空 path → 工作区根
	if got := fetchTree("").Path; filepath.Clean(got) != filepath.Clean(deps.dir) {
		t.Errorf("空 path 应返回工作区根：期望 %q，实际 %q", deps.dir, got)
	}
}

// TestStageFileInsidePassthrough 区内文件直接直通（staged_path=原路径，不复制）。
func TestStageFileInsidePassthrough(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	inside := filepath.Join(deps.dir, "notes.md")
	if err := os.WriteFile(inside, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": inside})
	var d struct {
		OK     bool   `json:"ok"`
		Inside bool   `json:"inside"`
		Path   string `json:"staged_path"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if !d.OK || !d.Inside {
		t.Fatalf("区内文件应直通: %+v", d)
	}
	// 不应产生 attachments 目录
	if _, err := os.Stat(filepath.Join(deps.dir, "attachments")); err == nil {
		t.Fatal("区内文件 stage 不应创建 attachments 副本")
	}
}
