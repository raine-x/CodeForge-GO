package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/agent"
	"codeforge/pkg/llm"
)

// serveDeps 用 deps 的 Routes() 直调一次请求。
// 不走 httptest.Server：这样能直接拿到 Server 实例与它的令牌，省掉一轮真实鉴权。
func serveDeps(t *testing.T, d *testDeps, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := d.newServer()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: TokenCookie, Value: s.Token()})
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// GET /api/sessions/rewind 列出回滚点
// ---------------------------------------------------------------------------

func TestRewindListEndpoint(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "回滚测试")
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	// 造一条可编辑的用户消息
	sess.Messages = append(sess.Messages, llm.TextMessage(llm.RoleUser, "帮我改文件"))
	if err := d.agent.History().Save(sess.ID); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	rec := serveDeps(t, d, http.MethodGet, "/api/sessions/rewind?id="+sess.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Steps     []agent.CheckpointStep `json:"steps"`
		Editables []agent.UserMessageRef `json:"editables"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Editables) != 1 || out.Editables[0].Text != "帮我改文件" {
		t.Fatalf("可编辑消息不符: %+v", out.Editables)
	}
}

func TestRewindListRequiresID(t *testing.T) {
	d := newTestDeps(t)
	rec := serveDeps(t, d, http.MethodGet, "/api/sessions/rewind", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，得到 %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// POST /api/sessions/rewind 回滚文件（Plan.md #4 验收：改坏 3 个文件 → 全部还原）
// ---------------------------------------------------------------------------

func TestRewindRestoresFilesViaAPI(t *testing.T) {
	d := newTestDeps(t)
	sess, err := d.agent.History().Create(d.dir, "回滚验收")
	if err != nil {
		t.Fatal(err)
	}

	// 三个文件先写好原内容，再造检查点，再「改坏」
	originals := map[string]string{"a.txt": "A原", "b.txt": "B原", "c.txt": "C原"}
	for name, body := range originals {
		p := filepath.Join(d.dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		d.agent.RecordCheckpoint(sess.ID, 1, p, true, body)
	}
	for name := range originals {
		_ = os.WriteFile(filepath.Join(d.dir, name), []byte("改坏了"), 0o644)
	}

	rec := serveDeps(t, d, http.MethodPost, "/api/sessions/rewind",
		`{"id":"`+sess.ID+`","to_step":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK     bool               `json:"ok"`
		Result agent.RewindResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !out.OK || out.Result.Failed != 0 {
		t.Fatalf("回滚应成功且无失败项: %+v", out)
	}
	for name, want := range originals {
		got, err := os.ReadFile(filepath.Join(d.dir, name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s 未还原：期望 %q，得到 %q", name, want, got)
		}
	}
}

func TestRewindPostRequiresID(t *testing.T) {
	d := newTestDeps(t)
	rec := serveDeps(t, d, http.MethodPost, "/api/sessions/rewind", `{"to_step":1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，得到 %d", rec.Code)
	}
}

func TestRewindMethodNotAllowed(t *testing.T) {
	d := newTestDeps(t)
	rec := serveDeps(t, d, http.MethodDelete, "/api/sessions/rewind?id=x", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("期望 405，得到 %d", rec.Code)
	}
}
