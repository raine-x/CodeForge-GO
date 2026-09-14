package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// isPosixPerm 报告当前平台是否支持 POSIX 权限位（Windows 上文件模式不可信）。
func isPosixPerm(mode os.FileMode) bool { return runtime.GOOS != "windows" }

// newTestStore 造一个临时目录里的空模型库。
func newTestStore(t *testing.T) *ModelStore {
	t.Helper()
	return NewModelStore(filepath.Join(t.TempDir(), "models.yaml"))
}

func TestModelStoreUpsertFindDelete(t *testing.T) {
	s := newTestStore(t)

	if err := s.Upsert(ModelEntry{ID: "a/model-1", Protocol: "custom", KeySource: "env", KeyName: "K"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// 同 ID 再插一次 = 更新，不产生重复条目。
	if err := s.Upsert(ModelEntry{ID: "A/MODEL-1", Protocol: "openai"}); err != nil {
		t.Fatalf("Upsert 大小写判重: %v", err)
	}
	if got := len(s.List()); got != 1 {
		t.Fatalf("期望 1 条（大小写不敏感判重），实际 %d 条", got)
	}
	m, ok := s.Find("a/MODEL-1")
	if !ok || m.Protocol != "openai" {
		t.Fatalf("Find 应命中更新后的条目，got %+v ok=%v", m, ok)
	}
	if !s.Delete("A/model-1") {
		t.Fatal("Delete 应成功")
	}
	if s.Delete("a/model-1") {
		t.Fatal("重复 Delete 应返回 false")
	}
	if err := s.Upsert(ModelEntry{ID: "  "}); err == nil {
		t.Fatal("空 ID 应报错")
	}
}

func TestModelStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")

	s1 := NewModelStore(path)
	if err := s1.Upsert(ModelEntry{
		ID: "z-ai/glm-5.3-free", Name: "GLM", Protocol: "custom",
		KeySource: "env", KeyName: "CODEFORGE_API_KEY",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// 文件权限：POSIX 上应为 0600（密钥落盘不放宽）；Windows 无 POSIX 权限位，跳过。
	if fi, err := os.Stat(path); err != nil {
		t.Fatalf("Stat: %v", err)
	} else if fi.Mode().Perm()&0o077 != 0 && os.Getenv("GOOS") == "" && filepath.VolumeName(path) == "" && isPosixPerm(fi.Mode().Perm()) {
		t.Fatalf("models.yaml 权限期望 0600，实际 %v", fi.Mode().Perm())
	}

	// 新实例重新加载后能读到同一条目（历史 custom 在 Upsert 时已归一化为 openai）。
	s2 := NewModelStore(path)
	m, ok := s2.Find("z-ai/glm-5.3-free")
	if !ok || m.Name != "GLM" || m.Protocol != "openai" {
		t.Fatalf("重载后条目不符: %+v ok=%v", m, ok)
	}
}

func TestModelStoreSanitizedHidesPlainKey(t *testing.T) {
	s := newTestStore(t)
	_ = s.Upsert(ModelEntry{ID: "m1", KeySource: "plain", KeyValue: "sk-SECRET", Protocol: "openai"})

	view := s.Sanitized()
	if len(view) != 1 {
		t.Fatalf("Sanitized 应返回 1 条，实际 %d", len(view))
	}
	for k, v := range view[0] {
		if s, _ := v.(string); s == "sk-SECRET" {
			t.Fatalf("脱敏视图泄露了明文 key（字段 %s）", k)
		}
	}
	if view[0]["key_set"] != true {
		t.Fatalf("key_set 应为 true（明文非空），实际 %v", view[0]["key_set"])
	}
}

// TestModelStoreSanitizedKeepsContextWindows 上下文上限必须随脱敏视图下发。
// 早前漏了 ctx_in/ctx_out，设置页表单只能显示前端写死的默认值（262144），
// 用户改过也看不到回显；上下文进度条也依赖这里拿到「模型窗口」。
func TestModelStoreSanitizedKeepsContextWindows(t *testing.T) {
	s := newTestStore(t)
	_ = s.Upsert(ModelEntry{ID: "m1", Protocol: "openai", CtxIn: 262144, CtxOut: 131072})

	view := s.Sanitized()
	if len(view) != 1 {
		t.Fatalf("Sanitized 应返回 1 条，实际 %d", len(view))
	}
	if got := view[0]["ctx_in"]; got != 262144 {
		t.Errorf("ctx_in = %v，期望 262144", got)
	}
	if got := view[0]["ctx_out"]; got != 131072 {
		t.Errorf("ctx_out = %v，期望 131072", got)
	}
}

func TestModelEntryDisplayName(t *testing.T) {
	if (ModelEntry{ID: "a/b", Name: "别名"}).DisplayName() != "别名" {
		t.Fatal("有 name 时应优先 name")
	}
	if (ModelEntry{ID: "a/b"}).DisplayName() != "a/b" {
		t.Fatal("无 name 时应回退 id")
	}
}

func TestNormalizeModelProtocol(t *testing.T) {
	cases := map[string]string{
		"openai": "openai", "Anthropic": "anthropic", "CUSTOM": "openai",
		"custom ": "openai", "": "openai", "weird": "openai",
	}
	for in, want := range cases {
		if got := NormalizeModelProtocol(in); got != want {
			t.Errorf("NormalizeModelProtocol(%q) = %q, want %q", in, got, want)
		}
	}
	if NormalizeModelProtocol(" Custom ") != "openai" {
		t.Fatal("历史 custom 应归一化为 openai（界面不再区分兼容网关）")
	}
}

// 空路径 = 纯内存库：Save/Load 都必须是无害的空操作。
//
// 这条守卫防的是一类真实事故：调用方拿到空 configDir 后拼出相对路径 "models.yaml"，
// 结果把文件写进进程的当前工作目录（曾在 pkg/server/ 里凭空出现 models.yaml）。
func TestModelStoreEmptyPathStaysInMemory(t *testing.T) {
	s := NewModelStore("")
	if s.Path() != "" {
		t.Fatalf("纯内存库 Path 期望空串，实际 %q", s.Path())
	}
	if err := s.Load(); err != nil {
		t.Fatalf("纯内存库 Load 应无错，实际 %v", err)
	}
	if err := s.Upsert(ModelEntry{ID: "m1", Name: "内存模型", Protocol: "openai"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("纯内存库 Save 应无错（不落盘），实际 %v", err)
	}

	// 条目仍在内存里可读，但不会在磁盘上留下任何痕迹。
	if _, ok := s.Find("m1"); !ok {
		t.Fatal("纯内存库应能读到刚写入的条目")
	}
	if got := s.Sanitized(); len(got) != 1 {
		t.Fatalf("纯内存库应能导出 1 条脱敏条目，实际 %d", len(got))
	}
	// 注意：这里不能直接 Stat("models.yaml") —— config/ 目录下本就有一个
	// 合法的模型库文件（config/models.yaml）。「不污染工作目录」的回归测试
	// 放在 pkg/server（那里出现 models.yaml 才是异常）。
}
