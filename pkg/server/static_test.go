package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 独立设置页（web/dist/settings）是内嵌静态资源的一部分：
// 目录索引与相对路径解析都经过 handleRoot，这里钉住这两条链路。
func TestHandleRootSettingsPage(t *testing.T) {
	s := &Server{token: "test-token"}

	// 目录请求不带末尾斜杠 → 先重定向补上，页面里的相对路径才不会解析错
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	s.handleRoot(rec, req)
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /settings 应 301 到 /settings/，实际 %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings/" {
		t.Fatalf("重定向位置应为 /settings/，实际 %q", loc)
	}

	// 目录索引直出设置页本体
	req = httptest.NewRequest(http.MethodGet, "/settings/", nil)
	rec = httptest.NewRecorder()
	s.handleRoot(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings/ 应 200，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("Content-Type 应为 text/html，实际 %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "CodeForge 设置") {
		t.Fatal("设置页 HTML 内容不符（未内嵌或嵌入位置变了）")
	}

	// 页面引用的静态资源按相对路径可被直接取到
	for _, asset := range []string{"/settings/app.js", "/settings/styles.css"} {
		req = httptest.NewRequest(http.MethodGet, asset, nil)
		rec = httptest.NewRecorder()
		s.handleRoot(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s 应 200，实际 %d", asset, rec.Code)
		}
	}
}
