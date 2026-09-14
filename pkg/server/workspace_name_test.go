package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchJSON 是 postJSON 的 PATCH 版本（models_handlers_test.go 里那个只做 POST）。
func patchJSON(t *testing.T, client *http.Client, url string, body map[string]any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s 失败: %v", url, err)
	}
	return resp
}

// 重命名项目 = 只改显示名。这条测试守护的正是历史事故：
// 早先 RenameWorkspace 直接 UPDATE sessions.workspace，把
// C:\...\Desktop\test 抹成 test、把中文名变成 ????，项目随即失去工作目录。
func TestWorkspaceRenameIsDisplayNameOnly(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDepsAt(t, cfgDir)
	ts := httptest.NewServer(d.newServer().Routes())
	defer ts.Close()
	client := withAuthClient(t, ts)

	before := d.agent.WorkDir()
	if before == "" {
		t.Fatal("前置条件：测试实例应有工作区")
	}

	resp := patchJSON(t, client, ts.URL+"/api/workspaces",
		map[string]any{"workspace": before, "new_name": "我的项目"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("重命名期望 200，实际 %d", resp.StatusCode)
	}

	// ① 会话归属键（= 磁盘路径）必须原样不动
	listResp, err := client.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer listResp.Body.Close()
	var list struct {
		Items []struct {
			Workspace     string `json:"workspace"`
			WorkspaceName string `json:"workspace_name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) == 0 {
		t.Fatal("会话列表不该为空")
	}
	if list.Items[0].Workspace != before {
		t.Errorf("⚠️ 重命名改写了工作区键：期望 %q，实际 %q", before, list.Items[0].Workspace)
	}
	if list.Items[0].WorkspaceName != "我的项目" {
		t.Errorf("显示名应为「我的项目」，实际 %q", list.Items[0].WorkspaceName)
	}

	// ② agent 的工作目录不能被顺手改掉
	if got := d.agent.WorkDir(); got != before {
		t.Errorf("⚠️ 重命名改动了 agent 工作目录：期望 %q，实际 %q", before, got)
	}

	// ③ 也不该顺手把配置写回 local.yaml（旧实现在这里 SetWorkDir(name) + Save）
	if _, err := os.Stat(filepath.Join(cfgDir, "local.yaml")); err == nil {
		t.Errorf("重命名不应写回 local.yaml（会把 work_dir 覆盖成显示名）")
	}

	// ④ 清空显示名 = 回落默认
	resp2 := patchJSON(t, client, ts.URL+"/api/workspaces",
		map[string]any{"workspace": before, "new_name": ""})
	_ = resp2.Body.Close()
	if got := d.agent.History().WorkspaceName(before); got != "" {
		t.Errorf("清空后显示名应为空串，实际 %q", got)
	}
}

// 运行时的目录校验必须与启动时同源（main.go：「配置的工作目录不存在或不是目录，
// 按未选择工作区处理」）。否则一个已被删除的目录（或历史遗留的坏分组键）会被静默
// 设成工作目录，文件工具随后全线报错，用户看不出是哪一步坏的。
func TestWorkspaceSwitchRejectsMissingDir(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDepsAt(t, cfgDir)
	ts := httptest.NewServer(d.newServer().Routes())
	defer ts.Close()
	client := withAuthClient(t, ts)

	before := d.agent.WorkDir()
	beforeRoot := d.fsys.Root()

	missing := filepath.Join(t.TempDir(), "已经删掉的目录")
	body, _ := json.Marshal(map[string]any{"path": missing})
	resp, err := client.Post(ts.URL+"/api/workspace", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("不存在的目录应返回 400，实际 %d", resp.StatusCode)
	}
	if got := d.agent.WorkDir(); got != before {
		t.Errorf("拒绝后不该改 agent 工作目录：期望 %q，实际 %q", before, got)
	}
	if got := d.fsys.Root(); got != beforeRoot {
		t.Errorf("拒绝后不该改 FS 根：期望 %q，实际 %q", beforeRoot, got)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, "local.yaml")); err == nil {
		t.Errorf("拒绝后不该写 local.yaml")
	}

	// 顺带确认「文件当目录」也要拒
	fileNotDir := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(fileNotDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	body2, _ := json.Marshal(map[string]any{"path": fileNotDir})
	resp2, err := client.Post(ts.URL+"/api/workspace", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("文件（非目录）应返回 400，实际 %d", resp2.StatusCode)
	}
}

// countItems 数一下某个列表接口返回多少条。
func countItems(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", url, err)
	}
	defer resp.Body.Close()
	var d struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return len(d.Items)
}

// 项目级归档 / 恢复必须是一对可逆操作：侧栏 ⋯「归档」一次点掉整组，
// 归档页的「恢复整个项目」得能一次点回来（`archive:false`）。
//
// ⚠️ 这条守的是 `Archive` 字段的**指针语义**：若退回裸 bool，`{archive:false}` 会被
// 当成「没传这个字段」而落到 400，项目级恢复就完全没法表达。
func TestWorkspaceArchiveRoundTrip(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDepsAt(t, cfgDir)
	ts := httptest.NewServer(d.newServer().Routes())
	defer ts.Close()
	client := withAuthClient(t, ts)

	ws := d.agent.WorkDir()
	active := func() int { return countItems(t, client, ts.URL+"/api/sessions") }
	archived := func() int { return countItems(t, client, ts.URL+"/api/sessions?archived=1") }

	if active() != 1 || archived() != 0 {
		t.Fatalf("前置：应有 1 条活跃 / 0 条归档，实际 %d/%d", active(), archived())
	}

	resp := patchJSON(t, client, ts.URL+"/api/workspaces", map[string]any{"workspace": ws, "archive": true})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("整组归档期望 200，实际 %d", resp.StatusCode)
	}
	if active() != 0 || archived() != 1 {
		t.Fatalf("归档后应 0 活跃 / 1 归档，实际 %d/%d", active(), archived())
	}

	resp2 := patchJSON(t, client, ts.URL+"/api/workspaces", map[string]any{"workspace": ws, "archive": false})
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("整组恢复期望 200（archive:false 不能被当成「没传」），实际 %d", resp2.StatusCode)
	}
	if active() != 1 || archived() != 0 {
		t.Fatalf("恢复后应 1 活跃 / 0 归档，实际 %d/%d", active(), archived())
	}
}

// 工作区必须是**绝对路径**。别的接口的相对路径语义是「相对工作区根」，但工作区根没法
// 相对自己 —— 放任相对路径会被 FS.SetRoot 的 filepath.Abs 静默按**进程 CWD** 解析
//（cf 是项目根、直接跑二进制又可能是别处），换个启动方式工作区就变了。
func TestWorkspaceSwitchRejectsRelativePath(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDepsAt(t, cfgDir)
	ts := httptest.NewServer(d.newServer().Routes())
	defer ts.Close()
	client := withAuthClient(t, ts)

	before := d.agent.WorkDir()
	resp := postJSON(t, client, ts.URL+"/api/workspace", map[string]any{"path": "sub"})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("相对路径应返回 400，实际 %d", resp.StatusCode)
	}
	if got := d.agent.WorkDir(); got != before {
		t.Errorf("拒绝后不该改工作目录：期望 %q，实际 %q", before, got)
	}
}

// 选中工作区要落盘（重启自动恢复）；而「新建项目」的 path="" 只是临时清空，
// 不能把用户原先选好的工作区从 local.yaml 里抹掉。
func TestWorkspaceSwitchPersistsButEmptyDoesNotClear(t *testing.T) {
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := newTestDepsAt(t, cfgDir)
	ts := httptest.NewServer(d.newServer().Routes())
	defer ts.Close()
	client := withAuthClient(t, ts)

	localPath := filepath.Join(cfgDir, "local.yaml")

	// 换到一个新目录 → 应写回 local.yaml
	another := t.TempDir()
	body, _ := json.Marshal(map[string]any{"path": another})
	resp, err := client.Post(ts.URL+"/api/workspace", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("切换工作区期望 200，实际 %d", resp.StatusCode)
	}
	saved, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("切换工作区后应写出 local.yaml: %v", err)
	}
	if !strings.Contains(string(saved), another) {
		t.Errorf("local.yaml 应记录新工作区 %q，实际内容:\n%s", another, saved)
	}

	// path=""（新建项目）→ 只清运行态，不覆盖已保存的工作区
	body2, _ := json.Marshal(map[string]any{"path": ""})
	resp2, err := client.Post(ts.URL+"/api/workspace", "application/json", bytes.NewReader(body2))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if got := d.agent.WorkDir(); got != "" {
		t.Errorf("空 path 应清掉运行态工作区，实际 %q", got)
	}
	after, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), another) {
		t.Errorf("⚠️「新建项目」不该抹掉已保存的工作区，local.yaml 现在:\n%s", after)
	}
}
