package main

import (
	"path/filepath"
	"strings"
	"testing"

	"codeforge/pkg/agent"
	"codeforge/pkg/store"
)

// 会话续跑（-continue / -resume）的定位与工作区落实。

func resumeHistory(t *testing.T) *agent.History {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return agent.NewHistory(st)
}

func mustCreate(t *testing.T, h *agent.History, ws, title string) *agent.Session {
	t.Helper()
	s, err := h.Create(ws, title)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveResumeTarget(t *testing.T) {
	h := resumeHistory(t)
	wsA, wsB := t.TempDir(), t.TempDir()
	sessA := mustCreate(t, h, wsA, "拼豆豆调颜色")
	sessB := mustCreate(t, h, wsB, "另一个项目")
	mustCreate(t, h, filepath.Join(wsA, "dup"), "重名")
	mustCreate(t, h, filepath.Join(wsA, "dup2"), "重名")

	cases := []struct {
		name    string
		ws      string
		target  string
		wantID  string
		wantErr string
	}{
		{"空参数即不续跑", wsA, "", "", ""},
		{"last 只取本工作区的会话", wsA, "last", sessA.ID, ""},
		{"last 不误取别的工作区", wsB, "last", sessB.ID, ""},
		{"last 无会话时报错", t.TempDir(), "last", "", "还没有会话可续跑"},
		{"未选工作区且无会话", "", "last", "", "尚未选择工作区"},
		{"完整 ID", wsB, sessA.ID, sessA.ID, ""},
		{"ID 前缀", wsB, sessA.ID[:6], sessA.ID, ""},
		{"标题", wsB, "另一个项目", sessB.ID, ""},
		{"找不到", wsB, "zzz-not-a-session", "", "找不到会话"},
		{"标题多义", wsB, "重名", "", "匹配到 2 条会话"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveResumeTarget(h, c.ws, c.target)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("期望报错含 %q，实际 err=%v got=%+v", c.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if c.wantID == "" {
				if got != nil {
					t.Fatalf("应为 nil，实际 %+v", got)
				}
				return
			}
			if got == nil || got.SessionID != c.wantID {
				t.Fatalf("定位错会话：期望 %s，实际 %+v", c.wantID, got)
			}
		})
	}
}

// 续跑必须连工作区一起回到会话所属的那个项目：
// 在 A 项目里接着谈 B 项目的对话，模型下一步改的就是错项目的文件。
func TestApplyResumeWorkspace(t *testing.T) {
	live := t.TempDir()
	missing := filepath.Join(t.TempDir(), "已被删除")

	if got := applyResumeWorkspace(live, &resumeTarget{Workspace: live}); got != live {
		t.Errorf("同一工作区不该改动，实际 %s", got)
	}
	if got := applyResumeWorkspace(live, &resumeTarget{Workspace: missing}); got != live {
		t.Errorf("目标工作区已不存在时应保持原样，实际 %s", got)
	}
	if got := applyResumeWorkspace(live, &resumeTarget{Workspace: ""}); got != "" {
		t.Errorf("会话本就无工作区时应回到未选择态，实际 %s", got)
	}
	other := t.TempDir()
	if got := applyResumeWorkspace(live, &resumeTarget{Workspace: other}); got != other {
		t.Errorf("应切到会话所属工作区 %s，实际 %s", other, got)
	}
}

func TestWithResumeParam(t *testing.T) {
	if got := withResumeParam("http://127.0.0.1:8420", nil); got != "http://127.0.0.1:8420" {
		t.Errorf("不续跑时地址不该带参数，实际 %s", got)
	}
	got := withResumeParam("http://127.0.0.1:8420", &resumeTarget{SessionID: "ab12/cd?34"})
	if got != "http://127.0.0.1:8420/?s=ab12%2Fcd%3F34" {
		t.Errorf("会话 ID 未正确编入地址：%s", got)
	}
}
