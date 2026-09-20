package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeforge/pkg/llm"
)

// POST /api/context/compress 的契约：前端要按状态码区分「点早了」与「真失败」。

func compressPOST(t *testing.T, client *http.Client, url string, body map[string]any) (int, map[string]any) {
	t.Helper()
	resp := postJSON(t, client, url, body)
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("响应不是 JSON：%s", raw)
		}
	}
	return resp.StatusCode, out
}

// seedSession 每次都新建一条独立会话：复用同一条会让上一个子用例推进的压缩游标
// 渗到下一个用例里（游标仍在范围内时 normalizeCompression 不会复位它）。
func seedSession(t *testing.T, deps *testDeps, turns int) string {
	t.Helper()
	sess, err := deps.agent.History().Create(deps.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < turns; i++ {
		sess.Messages = append(sess.Messages,
			llm.TextMessage(llm.RoleUser, strings.Repeat("问", 200)),
			llm.TextMessage(llm.RoleAssistant, strings.Repeat("答", 200)))
	}
	if err := deps.agent.History().Save(sess.ID); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func TestContextCompressEndpoint(t *testing.T) {
	deps := newTestDeps(t)
	ts := httptest.NewServer(deps.newServer().Routes())
	t.Cleanup(ts.Close)
	jar := steerJar(t, ts)
	client := &http.Client{Jar: jar}
	url := ts.URL + "/api/context/compress"

	t.Run("有较早历史时压缩成功", func(t *testing.T) {
		id := seedSession(t, deps, 8)
		status, body := compressPOST(t, client, url, map[string]any{"session_id": id})
		if status != http.StatusOK || body["ok"] != true {
			t.Fatalf("应 200 + ok，实际 %v %v", status, body)
		}
		if before, after := body["before"], body["after"]; !(after.(float64) < before.(float64)) {
			t.Errorf("压缩后占用应变小：%v → %v", before, after)
		}
		if body["summarized"].(float64) < 1 {
			t.Errorf("应报告并入条数：%v", body)
		}
	})

	t.Run("没有较早历史时返回 409", func(t *testing.T) {
		id := seedSession(t, deps, 0)
		status, body := compressPOST(t, client, url, map[string]any{"session_id": id})
		if status != http.StatusConflict {
			t.Fatalf("应 409，实际 %v", status)
		}
		if e, _ := body["error"].(string); !strings.Contains(e, "没有可压缩") {
			t.Errorf("409 应说明原因，实际: %v", body)
		}
	})

	t.Run("缺会话时退回当前工作区最近一条", func(t *testing.T) {
		// 只留一条会话：sessions.updated_at 是秒级，同一秒内建的多条会并到同一个排序值，
		// 那时 Latest() 取到哪条是不确定的，测不出「回退到最近一条」这件事。
		for _, m := range deps.agent.History().List(deps.dir, false) {
			if err := deps.agent.History().Delete(m.ID); err != nil {
				t.Fatal(err)
			}
		}
		id := seedSession(t, deps, 6)
		status, body := compressPOST(t, client, url, map[string]any{})
		if status != http.StatusOK {
			t.Fatalf("不带 session_id 也应能压缩当前工作区会话：%v %v", status, body)
		}
		if sid, _ := body["session_id"].(string); sid != id {
			t.Errorf("应回退到该工作区最近一条会话：%v ≠ %v", sid, id)
		}
	})

	t.Run("GET 不允许", func(t *testing.T) {
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("应 405，实际 %v", resp.StatusCode)
		}
	})
}
