package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestStores 造一对临时目录里的空库（模型库 + 供应商库）。
func newTestStores(t *testing.T) (*ModelStore, *ProviderStore) {
	t.Helper()
	dir := t.TempDir()
	return NewModelStore(filepath.Join(dir, "models.yaml")),
		NewProviderStore(filepath.Join(dir, "providers.yaml"))
}

func mustUpsertModel(t *testing.T, s *ModelStore, m ModelEntry) {
	t.Helper()
	if err := s.Upsert(m); err != nil {
		t.Fatalf("Upsert(%s): %v", m.ID, err)
	}
}

// 同一个 base_url + key 下的多条模型必须归并成一个供应商 —— 这正是本次改造的初衷：
// 原 models.yaml 里 Atria-Dawn-Preview 与 deepseek-v4-flash-0731 把地址和密钥抄了两遍。
func TestMigrateProvidersMergesSameConnection(t *testing.T) {
	models, providers := newTestStores(t)
	const base = "https://discovery-api.intern-ai.org.cn/v1"
	const key = "sk-66bebabc"
	mustUpsertModel(t, models, ModelEntry{ID: "Atria-Dawn-Preview", Name: "AD", BaseURL: base, Protocol: "openai", KeySource: "plain", KeyValue: key, CtxIn: 256000})
	mustUpsertModel(t, models, ModelEntry{ID: "deepseek-v4-flash-0731", BaseURL: base, Protocol: "openai", KeySource: "plain", KeyValue: key, CtxIn: 262144})
	mustUpsertModel(t, models, ModelEntry{ID: "gpt-4o", BaseURL: "https://api.openai.com/v1", Protocol: "openai", KeySource: "plain", KeyValue: "sk-other"})

	n, err := MigrateProviders(models, providers)
	if err != nil {
		t.Fatalf("MigrateProviders: %v", err)
	}
	if n != 3 {
		t.Fatalf("期望归并 3 条，实际 %d", n)
	}

	list := providers.List()
	if len(list) != 2 {
		t.Fatalf("期望 2 个供应商（两个不同网关），实际 %d: %+v", len(list), list)
	}

	// 归并后：连接信息在供应商上，条目上已清空（= 继承）。
	a, _ := models.Find("Atria-Dawn-Preview")
	b, _ := models.Find("deepseek-v4-flash-0731")
	if a.ProviderID == "" || a.ProviderID != b.ProviderID {
		t.Fatalf("同网关的两条应指向同一供应商，got %q / %q", a.ProviderID, b.ProviderID)
	}
	if a.BaseURL != "" || a.KeyValue != "" {
		t.Fatalf("归并后条目上的连接信息应清空，got base=%q key=%q", a.BaseURL, a.KeyValue)
	}
	// 上下文这类「模型自己的属性」必须原样保留。
	if a.CtxIn != 256000 || b.CtxIn != 262144 {
		t.Fatalf("上下文不应被迁移改动，got %d / %d", a.CtxIn, b.CtxIn)
	}

	p, ok := providers.Find(a.ProviderID)
	if !ok {
		t.Fatalf("供应商 %q 应存在", a.ProviderID)
	}
	if p.BaseURL != base || p.KeyValue != key {
		t.Fatalf("供应商连接信息不对: %+v", p)
	}
	if p.Name != "discovery-api.intern-ai.org.cn" {
		t.Fatalf("供应商名应由主机名推导，got %q", p.Name)
	}
}

// 迁移必须幂等：第二次调用不能把用户手工整理过的分组再拆一遍。
func TestMigrateProvidersIsIdempotent(t *testing.T) {
	models, providers := newTestStores(t)
	mustUpsertModel(t, models, ModelEntry{ID: "m1", BaseURL: "https://a.example.com/v1", Protocol: "openai", KeySource: "plain", KeyValue: "k"})
	if _, err := MigrateProviders(models, providers); err != nil {
		t.Fatalf("首次迁移: %v", err)
	}
	before := len(providers.List())

	n, err := MigrateProviders(models, providers)
	if err != nil {
		t.Fatalf("二次迁移: %v", err)
	}
	if n != 0 {
		t.Fatalf("二次迁移不应再归并，实际 %d 条", n)
	}
	if got := len(providers.List()); got != before {
		t.Fatalf("供应商数量不应变化，before=%d after=%d", before, got)
	}
}

// 没有 base_url 的条目无从归并（可能依赖官方默认端点），必须原样保留自带连接信息。
func TestMigrateProvidersSkipsEntriesWithoutBaseURL(t *testing.T) {
	models, providers := newTestStores(t)
	mustUpsertModel(t, models, ModelEntry{ID: "official", Protocol: "openai", KeySource: "env", KeyName: "OPENAI_API_KEY"})

	n, err := MigrateProviders(models, providers)
	if err != nil {
		t.Fatalf("MigrateProviders: %v", err)
	}
	if n != 0 || len(providers.List()) != 0 {
		t.Fatalf("无地址条目不应被归并，migrated=%d providers=%d", n, len(providers.List()))
	}
	m, _ := models.Find("official")
	if m.ProviderID != "" || m.KeyName != "OPENAI_API_KEY" {
		t.Fatalf("条目应保持自带连接信息，got %+v", m)
	}
}

// 继承解析：条目留空 → 取供应商；条目显式覆盖 → 以条目为准。
func TestResolveModelInheritsAndOverrides(t *testing.T) {
	models, providers := newTestStores(t)
	t.Setenv("PROVIDER_KEY", "env-secret")
	if err := providers.Upsert(Provider{
		ID: "p-a", Name: "A", BaseURL: "https://a.example.com/v1",
		Protocol: "anthropic", KeySource: "env", KeyName: "PROVIDER_KEY",
	}); err != nil {
		t.Fatalf("Upsert provider: %v", err)
	}

	// 完全继承
	inherited := ResolveModel(ModelEntry{ID: "m1", ProviderID: "p-a"}, providers)
	if inherited.BaseURL != "https://a.example.com/v1" || inherited.Protocol != "anthropic" ||
		inherited.KeyName != "PROVIDER_KEY" || inherited.KeySource != "env" {
		t.Fatalf("应整套继承供应商连接信息，got %+v", inherited)
	}
	if !EntryKeySet(inherited) {
		t.Fatal("继承来的 env 密钥已设置，EntryKeySet 应为 true")
	}

	// 条目自带明文密钥 → 覆盖供应商的 env 来源（三件套整体以条目为准）
	override := ResolveModel(ModelEntry{ID: "m2", ProviderID: "p-a", KeySource: "plain", KeyValue: "own-key"}, providers)
	if override.KeyValue != "own-key" || override.KeySource != "plain" || override.KeyName != "" {
		t.Fatalf("条目自带密钥应整体覆盖，got %+v", override)
	}
	if override.BaseURL != "https://a.example.com/v1" {
		t.Fatalf("地址仍应继承，got %q", override.BaseURL)
	}

	// 地址单独覆盖
	addr := ResolveModel(ModelEntry{ID: "m3", ProviderID: "p-a", BaseURL: "https://mirror.example.com/v1"}, providers)
	if addr.BaseURL != "https://mirror.example.com/v1" || addr.Protocol != "anthropic" {
		t.Fatalf("地址应被条目覆盖、协议仍继承，got %+v", addr)
	}
	_ = models

	// 指向不存在的供应商 → 原样返回，协议兜底 openai（旧格式兼容）
	orphan := ResolveModel(ModelEntry{ID: "m4", ProviderID: "p-missing"}, providers)
	if orphan.Protocol != "openai" || orphan.BaseURL != "" {
		t.Fatalf("找不到供应商时应原样返回并兜底协议，got %+v", orphan)
	}
	// ProviderID 为空的旧格式条目完全不受影响
	legacy := ResolveModel(ModelEntry{ID: "m5", BaseURL: "https://old.example.com/v1", Protocol: "custom"}, providers)
	if legacy.Protocol != "openai" || legacy.BaseURL != "https://old.example.com/v1" {
		t.Fatalf("旧格式条目应保持可用（custom→openai），got %+v", legacy)
	}
}

// 供应商的明文密钥绝不能被序列化进 JSON。
func TestProviderSanitizedHidesPlainKey(t *testing.T) {
	_, providers := newTestStores(t)
	if err := providers.Upsert(Provider{
		ID: "p-a", Name: "A", BaseURL: "https://a.example.com/v1",
		Protocol: "openai", KeySource: "plain", KeyValue: "sk-secret",
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	out := providers.Sanitized()
	if len(out) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(out))
	}
	if _, leaked := out[0]["key_value"]; leaked {
		t.Fatal("脱敏视图不得包含 key_value")
	}
	for k, v := range out[0] {
		if s, ok := v.(string); ok && strings.Contains(s, "sk-secret") {
			t.Fatalf("脱敏视图字段 %s 泄漏了明文密钥", k)
		}
	}
	if out[0]["key_set"] != true {
		t.Fatalf("key_set 应为 true，got %v", out[0]["key_set"])
	}
	if out[0]["disabled"] != false {
		t.Fatalf("默认应为启用，got %v", out[0]["disabled"])
	}
}

// 供应商库落盘后可原样读回（含明文密钥，供重启后继续使用）。
func TestProviderStorePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.yaml")

	p1 := NewProviderStore(path)
	if err := p1.Upsert(Provider{
		ID: "p-a", Name: "A", BaseURL: "https://a.example.com/v1/",
		Protocol: "custom", KeySource: "plain", KeyValue: "sk-1", Disabled: true,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := p1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	p2 := NewProviderStore(path)
	if err := p2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	got, ok := p2.Find("P-A") // 大小写不敏感
	if !ok {
		t.Fatal("Find 应命中")
	}
	if got.BaseURL != "https://a.example.com/v1" {
		t.Fatalf("地址尾部斜杠应被规整，got %q", got.BaseURL)
	}
	if got.Protocol != "openai" {
		t.Fatalf("custom 应归一化为 openai，got %q", got.Protocol)
	}
	if got.KeyValue != "sk-1" || !got.Disabled {
		t.Fatalf("密钥与停用标记应完整往返，got %+v", got)
	}
}

// 明文值非空却标着 env 只会让密钥静默失效，一律判为 plain。
func TestNormalizeKeySource(t *testing.T) {
	cases := []struct{ source, value, want string }{
		{"env", "sk-x", "plain"},
		{"plain", "", "plain"},
		{"env", "", "env"},
		{"", "", "env"},
		{"PLAIN", "", "plain"},
	}
	for _, c := range cases {
		if got := NormalizeKeySource(c.source, c.value); got != c.want {
			t.Errorf("NormalizeKeySource(%q,%q) = %q, 期望 %q", c.source, c.value, got, c.want)
		}
	}
}

func TestProviderNameFromBase(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.deepseek.com", "deepseek.com"},
		{"https://open.bigmodel.cn/api/paas/v4", "open.bigmodel.cn"},
		{"https://discovery-api.intern-ai.org.cn/v1", "discovery-api.intern-ai.org.cn"},
		{"", "未命名供应商"},
		{"not-a-url", "not-a-url"},
	}
	for _, c := range cases {
		if got := providerNameFromBase(c.base); got != c.want {
			t.Errorf("providerNameFromBase(%q) = %q, 期望 %q", c.base, got, c.want)
		}
	}
}

// 归并后的库整体往返一次磁盘：模型条目丢掉 base_url 后，仍能从供应商解析出来。
func TestMigratedLibrarySurvivesReload(t *testing.T) {
	dir := t.TempDir()
	modelsPath := filepath.Join(dir, "models.yaml")
	providersPath := filepath.Join(dir, "providers.yaml")

	m1 := NewModelStore(modelsPath)
	p1 := NewProviderStore(providersPath)
	mustUpsertModel(t, m1, ModelEntry{ID: "m1", BaseURL: "https://a.example.com/v1", Protocol: "openai", KeySource: "plain", KeyValue: "sk-1"})
	mustUpsertModel(t, m1, ModelEntry{ID: "m2", BaseURL: "https://a.example.com/v1", Protocol: "openai", KeySource: "plain", KeyValue: "sk-1"})
	if _, err := MigrateProviders(m1, p1); err != nil {
		t.Fatalf("迁移: %v", err)
	}

	// 模拟进程重启：重新从磁盘加载两个库。
	m2 := NewModelStore(modelsPath)
	p2 := NewProviderStore(providersPath)
	if err := m2.Load(); err != nil {
		t.Fatalf("重载模型库: %v", err)
	}
	if err := p2.Load(); err != nil {
		t.Fatalf("重载供应商库: %v", err)
	}

	resolved := m2.SanitizedResolved(p2)
	if len(resolved) != 2 {
		t.Fatalf("期望 2 条模型，实际 %d", len(resolved))
	}
	for _, r := range resolved {
		if r["base_url"] != "https://a.example.com/v1" {
			t.Fatalf("重启后应仍能解析出地址，got %v", r["base_url"])
		}
		if r["key_set"] != true {
			t.Fatalf("重启后应仍能解析出密钥，got %v", r["key_set"])
		}
		if r["provider_id"] == "" {
			t.Fatal("provider_id 应随条目落盘")
		}
	}

	// 未解析视图必须保持「留空」—— 它是继承语义的载体，不能被悄悄填上。
	if raw := m2.Sanitized(); raw[0]["base_url"] != "" {
		t.Fatalf("原始视图的 base_url 应保持为空，got %v", raw[0]["base_url"])
	}
}

func TestUniqueProviderIDDedupes(t *testing.T) {
	used := map[string]bool{}
	if got := uniqueProviderID("a.example.com", used); got != "p-a-example-com" {
		t.Fatalf("got %q", got)
	}
	if got := uniqueProviderID("a.example.com", used); got != "p-a-example-com-2" {
		t.Fatalf("重名应追加序号，got %q", got)
	}
	if got := uniqueProviderID("a.example.com", used); got != "p-a-example-com-3" {
		t.Fatalf("got %q", got)
	}
	if got := uniqueProviderID("中文", used); got != "p-provider" {
		t.Fatalf("无法生成 slug 时应兜底，got %q", got)
	}
}

func TestProviderStoreInMemoryNeverWritesRelativePath(t *testing.T) {
	// path 为空 = 纯内存库：Save 不得退化成写相对路径 "providers.yaml"。
	//
	// ⚠️ 必须在**空的临时目录**里跑。
	// 本测试的 CWD 默认是 config/，而应用**本来就会**往那里写 config/providers.yaml ——
	// 只要跑过一次应用，`os.Stat("providers.yaml")` 就会命中那个正常文件，
	// 断言「纯内存库不应落盘」直接假失败（2026-09-20 实际踩到）。
	// 换个干净目录，「文件是否存在」才真正等价于「本次 Save 有没有落盘」。
	t.Chdir(t.TempDir())

	s := NewProviderStore("")
	if err := s.Upsert(Provider{ID: "p-a", Name: "A"}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat("providers.yaml"); err == nil {
		t.Fatal("纯内存库不应落盘")
	}
}
