package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// diffResp 是 /api/diff 的响应体。
type diffResp struct {
	Path    string `json:"path"`
	Step    int    `json:"step"`
	Existed bool   `json:"existed"`
	Diff    string `json:"diff"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Compare string `json:"compare"`
	Note    string `json:"note"`
}

func getDiff(t *testing.T, d *testDeps, sessionID, path, step string) diffResp {
	t.Helper()
	target := "/api/diff?session_id=" + url.QueryEscape(sessionID) + "&path=" + url.QueryEscape(path)
	if step != "" {
		target += "&step=" + url.QueryEscape(step)
	}
	rec := serveDeps(t, d, http.MethodGet, target, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out diffResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v (%s)", err, rec.Body.String())
	}
	return out
}

// 有检查点、文件还在：给出「改动前 → 现在」的 diff 与行数。
func TestDiffEndpointWithCheckpoint(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "diff 测试")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	target := filepath.Join(d.dir, "note.txt")
	const old = "第一行\n第二行\n第三行\n"
	const cur = "第一行\n第二行改过\n第三行\n"
	if err := os.WriteFile(target, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	// 第 3 步改写了这个文件，改动前内容是 old。
	d.agent.RecordCheckpoint(sess.ID, 3, target, true, old)
	if err := os.WriteFile(target, []byte(cur), 0o644); err != nil {
		t.Fatal(err)
	}

	out := getDiff(t, d, sess.ID, target, "")
	if !strings.Contains(out.Diff, "-第二行") || !strings.Contains(out.Diff, "+第二行改过") {
		t.Errorf("diff 应包含这次改动，实际：\n%s", out.Diff)
	}
	if out.Added != 1 || out.Removed != 1 {
		t.Errorf("期望 +1/-1，实际 +%d/-%d", out.Added, out.Removed)
	}
	if !out.Existed {
		t.Error("existed 应为 true（改动前文件就存在）")
	}
	if out.Compare != "current" {
		t.Errorf("compare 应为 current（与当前文件对比），实际 %q", out.Compare)
	}
	if out.Note != "" {
		t.Errorf("有 diff 时不该带 note，实际 %q", out.Note)
	}

	// 指定 step 也应命中同一条。
	if got := getDiff(t, d, sess.ID, target, "3"); got.Diff != out.Diff {
		t.Errorf("按 step=3 查询应命中同一条检查点")
	}
	// 指定了不存在的 step → 查不到，给说明而不是空 diff。
	if got := getDiff(t, d, sess.ID, target, "99"); got.Note == "" {
		t.Error("step 不匹配时应给出说明")
	}
}

// 新建文件（改动前不存在）：diff 全是新增。
func TestDiffEndpointNewFile(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "diff 新建")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	target := filepath.Join(d.dir, "created.txt")
	d.agent.RecordCheckpoint(sess.ID, 1, target, false, "") // existed=false
	if err := os.WriteFile(target, []byte("a\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := getDiff(t, d, sess.ID, target, "")
	if out.Existed {
		t.Error("existed 应为 false（改动前不存在）")
	}
	if out.Added != 2 || out.Removed != 0 {
		t.Errorf("新建文件应报 +2/-0，实际 +%d/-%d", out.Added, out.Removed)
	}
}

// 查不到检查点：明确说明，而不是静默返回空 diff。
// 这同时是「只能读本会话改过的文件」这条约束的体现。
func TestDiffEndpointUnknownPath(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "diff 未知路径")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	out := getDiff(t, d, sess.ID, filepath.Join(d.dir, "never-touched.txt"), "")
	if out.Diff != "" {
		t.Errorf("未知路径不该给出 diff，实际 %q", out.Diff)
	}
	if out.Note == "" {
		t.Error("未知路径应给出说明")
	}
}

// 改动前存在、现在读不到（被删/移走）：如实说明，别把 diff 算成整份删除。
func TestDiffEndpointFileGone(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "diff 文件已删")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	target := filepath.Join(d.dir, "gone.txt")
	d.agent.RecordCheckpoint(sess.ID, 2, target, true, "原来有内容\n")
	// 不创建文件 —— 模拟改动之后文件被删掉。

	out := getDiff(t, d, sess.ID, target, "")
	if out.Diff != "" {
		t.Errorf("文件读不到时不该编造 diff，实际 %q", out.Diff)
	}
	if out.Note == "" {
		t.Error("应给出「文件读不到」的说明")
	}
}
