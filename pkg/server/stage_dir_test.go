// stage_dir_test.go 覆盖「文件夹添加」：区外目录整棵复制到 attachments/。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTreeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStageDirectoryCopiesTree 区外目录应保持内部结构整棵复制进 attachments/。
func TestStageDirectoryCopiesTree(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	outside := t.TempDir()
	src := filepath.Join(outside, "myproj")
	writeTreeFile(t, src, "a.go", "package a")
	writeTreeFile(t, src, filepath.Join("sub", "b.go"), "package b")
	writeTreeFile(t, src, filepath.Join("sub", "deep", "c.txt"), "deep content")

	resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": src})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stage 目录失败: HTTP %d", resp.StatusCode)
	}
	var d struct {
		OK         bool   `json:"ok"`
		IsDir      bool   `json:"is_dir"`
		Count      int    `json:"count"`
		StagedPath string `json:"staged_path"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if !d.OK || !d.IsDir {
		t.Fatalf("应识别为目录: %+v", d)
	}
	if d.Count != 3 {
		t.Errorf("应复制 3 个文件，实际 %d", d.Count)
	}
	if d.Name != "myproj" {
		t.Errorf("name 应为目录名，实际 %q", d.Name)
	}
	// 结构保持：attachments/myproj/sub/deep/c.txt 内容一致。
	staged := filepath.Join(deps.dir, filepath.FromSlash(d.StagedPath))
	for rel, want := range map[string]string{
		"a.go":                                 "package a",
		filepath.Join("sub", "b.go"):          "package b",
		filepath.Join("sub", "deep", "c.txt"): "deep content",
	} {
		got, err := os.ReadFile(filepath.Join(staged, rel))
		if err != nil || string(got) != want {
			t.Errorf("staged 文件 %s 内容不符: %v %q", rel, err, got)
		}
	}
}

// TestStageDirectoryIdempotent 重复 stage 同一目录：同名同内容复用，不报错也不翻倍。
func TestStageDirectoryIdempotent(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	src := filepath.Join(t.TempDir(), "data")
	writeTreeFile(t, src, "f.txt", "same")

	for i := 0; i < 2; i++ {
		resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": src})
		var d struct {
			OK    bool `json:"ok"`
			IsDir bool `json:"is_dir"`
			Count int  `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if !d.OK || !d.IsDir || d.Count != 1 {
			t.Fatalf("第 %d 次 stage 异常: %+v", i+1, d)
		}
	}
}

// TestStageDirectoryMissing 不存在的路径应 400。
func TestStageDirectoryMissing(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	client := withAuthClient(t, ts)

	resp := postJSON(t, client, ts.URL+"/api/stage_file", map[string]any{"path": filepath.Join(t.TempDir(), "nope")})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不存在路径应 400，实际 %d", resp.StatusCode)
	}
}
