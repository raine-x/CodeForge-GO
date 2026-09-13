package builtin

import (
	"strings"
	"testing"
)

func TestUnifiedDiffBasic(t *testing.T) {
	oldText := "line1\nline2\nline3\nline4\n"
	newText := "line1\nline2-changed\nline3\nline4\n"
	diff := UnifiedDiff("a.txt", "a.txt", oldText, newText)

	if diff == "" {
		t.Fatal("期望产生差异，实际为空")
	}
	if !strings.Contains(diff, "-line2") {
		t.Errorf("差异中缺少删除行: %s", diff)
	}
	if !strings.Contains(diff, "+line2-changed") {
		t.Errorf("差异中缺少新增行: %s", diff)
	}
	if !strings.Contains(diff, "--- a.txt") || !strings.Contains(diff, "+++ a.txt") {
		t.Errorf("差异缺少文件头: %s", diff)
	}
}

func TestUnifiedDiffNoChange(t *testing.T) {
	text := "same\ncontent\n"
	if diff := UnifiedDiff("a", "a", text, text); diff != "" {
		t.Fatalf("无差异时应返回空字符串，实际: %q", diff)
	}
}

func TestUnifiedDiffEmptyToContent(t *testing.T) {
	diff := UnifiedDiff("new.txt", "new.txt", "", "hello\n")
	if !strings.Contains(diff, "+hello") {
		t.Fatalf("新增文件差异不正确: %s", diff)
	}
}

func TestUnifiedDiffHunkHeader(t *testing.T) {
	var oldLines, newLines []string
	for i := 1; i <= 20; i++ {
		oldLines = append(oldLines, "L"+itoa(i))
		newLines = append(newLines, "L"+itoa(i))
	}
	newLines[10] = "CHANGED"

	diff := UnifiedDiff("f", "f", strings.Join(oldLines, "\n")+"\n", strings.Join(newLines, "\n")+"\n")
	if !strings.Contains(diff, "@@") {
		t.Fatalf("缺少 hunk 头: %s", diff)
	}
	if strings.Count(diff, "@@") != 2 {
		t.Errorf("期望单个 hunk（2 个 @@ 标记），实际: %s", diff)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
