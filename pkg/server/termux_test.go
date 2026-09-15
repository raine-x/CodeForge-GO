package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Termux 工具安装建议接口（termux.go）。
//
// 本机（Windows / 非 Termux）只能钉住「平台不对时诚实报告」这一半：
//   - GET 返回 termux=false（前端据此绝不弹窗）；
//   - POST 拒绝安装（400）。
// 「Termux 且未装 → 安装 → 装好」的完整链路依赖真实 Termux 环境，
// 状态机本身很简单（atomic CAS + LookPath 复核），交给真机验证。

func TestTermuxToolsReportsNonTermux(t *testing.T) {
	srv := newTestDeps(t).newServer()

	rec := httptest.NewRecorder()
	srv.handleTermuxTools(rec, httptest.NewRequest(http.MethodGet, "/api/termux/tools", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if want := `"termux":false`; !strings.Contains(body, want) {
		t.Errorf("GET 响应应含 %s（非 Termux 平台必须如实报告），实际 %s", want, body)
	}
	if strings.Contains(body, `"installed":true`) {
		t.Errorf("非 Termux 平台不应报告 installed=true，实际 %s", body)
	}
}

func TestTermuxToolsPostRejectedOnNonTermux(t *testing.T) {
	srv := newTestDeps(t).newServer()

	rec := httptest.NewRecorder()
	srv.handleTermuxTools(rec, httptest.NewRequest(http.MethodPost, "/api/termux/tools", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST status = %d，期望 400（非 Termux 平台拒绝安装）", rec.Code)
	}
	// 拒绝后状态必须仍为 idle（不能卡在 installing）
	if st := srv.termuxState.Load(); st != termuxIdle {
		t.Errorf("拒绝安装后 termuxState = %d，期望 %d（idle）", st, termuxIdle)
	}
}
