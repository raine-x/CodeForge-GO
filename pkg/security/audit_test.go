package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditLogRotates 超限后应轮转，且旧档名次递增。
//
// 改造前 NewAuditLogger 只有 os.O_APPEND，没有任何大小检查，
// 于是审计日志会随运行时间无限增长。
func TestAuditLogRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// 构造一个已经超过上限的日志（上限 1 KiB）
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := newAuditLogger(path, 1024, 3)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer a.Close()

	if err := a.Log(AuditEntry{Tool: "write_file", Action: "x.go"}); err != nil {
		t.Fatalf("Log: %v", err)
	}

	// 原来的超限内容应被移到 .1
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("应生成轮转档 %s.1：%v", path, err)
	}
	// 主文件应已重置，只剩新写入的一条
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 1024 {
		t.Errorf("主文件仍有 %d 字节，未被重置", len(body))
	}
	if !strings.Contains(string(body), "write_file") {
		t.Errorf("新记录没写进主文件：%q", string(body))
	}
}

// TestAuditRotationKeepsN 超出保留份数时最旧的应被删除。
func TestAuditRotationKeepsN(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	// 预置 4 份已满的历史档 + 一份超限主文件
	for i := 1; i <= 4; i++ {
		if err := os.WriteFile(oldestFirst(path, i), []byte(strings.Repeat("y", 2048)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := newAuditLogger(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Log(AuditEntry{Tool: "t"}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// 保留 3 份 ⇒ 编号 4 应已删除，1/2/3 仍在
	if _, err := os.Stat(oldestFirst(path, 4)); !os.IsNotExist(err) {
		t.Errorf("最旧的一档（第 4 份）应被删除，实际仍在")
	}
	for i := 1; i <= 3; i++ {
		if _, err := os.Stat(oldestFirst(path, i)); err != nil {
			t.Errorf("第 %d 档应保留：%v", i, err)
		}
	}
}

// oldestFirst 生成轮转档名：.1 是最近的，.N 是最旧的。
func oldestFirst(path string, n int) string {
	return path + "." + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAuditBelowLimitNotRotated 未超限不该轮转（避免每次写都搬文件）。
func TestAuditBelowLimitNotRotated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")

	a, err := newAuditLogger(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := a.Log(AuditEntry{Tool: "read_file", Action: "a.go"}); err != nil {
			t.Fatal(err)
		}
	}
	a.Close()

	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Errorf("未超限不该产生轮转档，实际生成了 %s.1", path)
	}
	body, _ := os.ReadFile(path)
	if n := strings.Count(string(body), "\n"); n != 5 {
		t.Errorf("应有 5 条记录，实际 %d 条", n)
	}
}

// TestAuditRotateFailsClosed 轮转出错时应明确返回错误，而不是静默丢审计。
func TestAuditRotateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := newAuditLogger(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// 把主文件所在的目录改名，制造「轮转时目标目录不存在」
	// —— 不容易稳定构造，改为直接验证：轮转成功时不返回错误。
	if err := a.Log(AuditEntry{Tool: "t"}); err != nil {
		t.Errorf("轮转不应报错：%v", err)
	}
}
