package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeforge/config"
)

// 背景图选择链路的回归测试。
//
// 这里锁的是一个真实故障（2026-09-19 用户报「选完图片啥也没有」）：
// Windows 分支只把对话框选中的路径回给前端，**没有写 cfg.Appearance.BackgroundPath**，
// 而 /api/appearance/background 正是读这个字段 —— 于是永远 404，
// 图层虽然 .on 了却没有图可画，界面上一点反应都没有（也不报错，所以极难自查）。
//
// 关键断言是「落库」那一条：只测 HTTP 返回体是不够的，
// 必须同时证明 cfg 被改写 **且** 写回了运行状态（state.yaml）。

// setupAppearance 建一个绑定真实配置目录的 Server。
// 旧实现要求 ConfigDir 非空（否则写回会被跳过）；现在运行状态落在用户级
// state.yaml，与配置目录无关，ConfigDir 保留是因为背景图仍要读工作区外的本地文件。
func setupAppearance(t *testing.T) (*Server, string) {
	t.Helper()
	cfgDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgDir, "default.yaml"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("写入 default.yaml 失败: %v", err)
	}
	return newTestDepsAt(t, cfgDir).newServer(), cfgDir
}

// makeImage 造一个图片文件。内容本身不重要 —— /api/appearance/background 先按扩展名
// 定 Content-Type，这里验证的就是这条链路。
func makeImage(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("\x89PNG\r\n\x1a\nfake-image-body"), 0o644); err != nil {
		t.Fatalf("写入测试图失败: %v", err)
	}
	return p
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%q)", err, rec.Body.String())
	}
	return m
}

func TestValidatePick(t *testing.T) {
	bad := string([]byte{0xff, 0xfe})

	cases := []struct {
		name     string
		path     string
		err      error
		wantPath string
		wantMsg  string // 期望错误文案包含的子串（空 = 不应报错）
	}{
		{"正常路径", `C:\pics\a.png`, nil, `C:\pics\a.png`, ""},
		{"用户取消（空路径）", "", nil, "", ""},
		{"对话框失败", "", errors.New("exit status 1"), "", "打开系统对话框失败"},
		{"非法 UTF-8", bad, nil, "", "UTF-8"},
		{"有错误时路径被丢弃", `C:\a.png`, errors.New("boom"), "", "打开系统对话框失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg := validatePick(c.path, c.err, "背景图")
			if got != c.wantPath {
				t.Fatalf("path: 期望 %q 实际 %q", c.wantPath, got)
			}
			if c.wantMsg == "" {
				if msg != "" {
					t.Fatalf("不应报错，却得到 %q", msg)
				}
				return
			}
			if !strings.Contains(msg, c.wantMsg) {
				t.Fatalf("错误文案应包含 %q，实际 %q", c.wantMsg, msg)
			}
		})
	}
}

// TestFinishBackgroundPickPersists 是本次故障的核心回归防线：
// 选中的路径必须同时进入 cfg 与 local.yaml，否则 /api/appearance/background 会一直 404。
func TestFinishBackgroundPickPersists(t *testing.T) {
	s, _ := setupAppearance(t)
	img := makeImage(t, t.TempDir(), "bg.png")

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, img, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["ok"] != true || body["background"] != true {
		t.Fatalf("应返回 ok/background 均为 true，实际 %v", body)
	}
	if body["path"] != img {
		t.Fatalf("path 应为 %q，实际 %v", img, body["path"])
	}

	// ① 内存里的 cfg 必须被改写 —— 缺了这一步，前端拿到的 path 根本没法被服务出来
	if s.cfg.Appearance.BackgroundPath != img {
		t.Fatalf("cfg.Appearance.BackgroundPath 未被写入：实际 %q（这正是「选完图啥也没有」的根因）",
			s.cfg.Appearance.BackgroundPath)
	}

	// ② 必须真的写回磁盘（否则重启就丢，且 GET 会报 background_set:false）
	raw, err := os.ReadFile(config.StatePath())
	if err != nil {
		t.Fatalf("读取 state.yaml 失败: %v", err)
	}
	if !strings.Contains(string(raw), "background_path") {
		t.Fatalf("state.yaml 未写入 background_path：\n%s", raw)
	}

	// ③ GET /api/appearance 应报告已设置
	rec2 := httptest.NewRecorder()
	s.handleAppearance(rec2, httptest.NewRequest(http.MethodGet, "/api/appearance", nil))
	if decodeBody(t, rec2)["background_set"] != true {
		t.Fatalf("background_set 应为 true，实际 %v", decodeBody(t, rec2))
	}

	// ④ 背景图内容真的服务得出来（这是前端 applyBgImage 要拉的接口）
	rec3 := httptest.NewRecorder()
	s.handleAppearanceBackground(rec3, httptest.NewRequest(http.MethodGet, "/api/appearance/background", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("背景图接口应为 200，实际 %d（未落库时这里就是 404）", rec3.Code)
	}
	if ct := rec3.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，实际 %q", ct)
	}
	if !strings.Contains(rec3.Body.String(), "fake-image-body") {
		t.Fatalf("响应体不是所选图片的内容：%q", rec3.Body.String())
	}
}

func TestFinishBackgroundPickUserCancel(t *testing.T) {
	s, _ := setupAppearance(t)
	before := s.cfg.Appearance.BackgroundPath

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, "", nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("取消应返回 200，实际 %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["ok"] != false {
		t.Fatalf("取消应返回 ok=false，实际 %v", body)
	}
	// 取消不能清掉已有背景（用户点开又反悔，不该把原图弄丢）
	if s.cfg.Appearance.BackgroundPath != before {
		t.Fatalf("取消不应改动已有背景：%q → %q", before, s.cfg.Appearance.BackgroundPath)
	}
}

func TestFinishBackgroundPickDialogError(t *testing.T) {
	s, _ := setupAppearance(t)

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, "", errors.New("exit status 1"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("对话框失败应返回 500，实际 %d", rec.Code)
	}
	if !strings.Contains(decodeBody(t, rec)["error"].(string), "打开系统对话框失败") {
		t.Fatalf("错误文案不对：%s", rec.Body.String())
	}
	if s.cfg.Appearance.BackgroundPath != "" {
		t.Fatal("失败时不应写入背景路径")
	}
}

func TestFinishBackgroundPickRejectsMissingFile(t *testing.T) {
	s, _ := setupAppearance(t)

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, filepath.Join(t.TempDir(), "nope.png"), nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("文件不存在应返回 400，实际 %d", rec.Code)
	}
	if s.cfg.Appearance.BackgroundPath != "" {
		t.Fatal("文件不存在时不应写入背景路径")
	}
}

func TestFinishBackgroundPickRejectsNonImage(t *testing.T) {
	s, _ := setupAppearance(t)
	nonImage := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(nonImage, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, nonImage, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非图片文件应返回 400，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(decodeBody(t, rec)["error"].(string), "图片文件") {
		t.Fatalf("错误文案应说明只能选择图片：%s", rec.Body.String())
	}
	if s.cfg.Appearance.BackgroundPath != "" {
		t.Fatal("非图片文件不应写入背景路径")
	}
}

// TestFinishBackgroundPickRejectsInvalidUTF8 保证损坏路径不会一路写进配置。
func TestFinishBackgroundPickRejectsInvalidUTF8(t *testing.T) {
	s, _ := setupAppearance(t)

	rec := httptest.NewRecorder()
	s.finishBackgroundPick(rec, string([]byte{0xff, 0xfe}), nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("非法 UTF-8 应返回 500，实际 %d", rec.Code)
	}
	if s.cfg.Appearance.BackgroundPath != "" {
		t.Fatal("非法 UTF-8 路径不应被写入")
	}
}

// TestFinishPickDoesNotPersist 钉住 finishPick 的语义边界：
// 它是「只回报不落库」的通用收尾，用于选文件/选目录；落库是背景图专有的行为。
// 若哪天有人把落库塞进 finishPick，这条会提醒他重新考虑影响面。
func TestFinishPickDoesNotPersist(t *testing.T) {
	s, _ := setupAppearance(t)
	img := makeImage(t, t.TempDir(), "bg.png")

	rec := httptest.NewRecorder()
	s.finishPick(rec, img, nil, "文件")

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	if decodeBody(t, rec)["background"] != nil {
		t.Fatal("finishPick 不应带 background 字段（那是背景图专有的落库确认信号）")
	}
	if s.cfg.Appearance.BackgroundPath != "" {
		t.Fatal("finishPick 不应改动背景图配置")
	}
}
