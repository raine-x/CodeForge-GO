package agent

import (
	"path/filepath"
	"sort"
	"testing"

	"codeforge/pkg/store"
)

// 会话删除钩子：删除会话时要连带清掉该会话的工具结果外置目录，
// 否则一次 APK 逆向留下的几百 MB 会一直躺在盘上。
func TestHistoryDeleteFiresOnDelete(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHistory(st)

	var fired []string
	h.SetOnDelete(func(id string) { fired = append(fired, id) })

	sess, err := h.Create("", "待删")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 1 || fired[0] != sess.ID {
		t.Fatalf("删除单会话应触发一次钩子，实际 %v", fired)
	}
}

// 删工作区时要为**该工作区下的每个**会话触发一次，且不牵连别的工作区。
func TestHistoryDeleteWorkspaceFiresPerSession(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHistory(st)

	a1, _ := h.Create("/proj/A", "a1")
	a2, _ := h.Create("/proj/A", "a2")
	b1, _ := h.Create("/proj/B", "b1")
	// 取一次让它们进缓存 —— DeleteWorkspace 只遍历缓存。
	for _, id := range []string{a1.ID, a2.ID, b1.ID} {
		if _, ok := h.Get(id); !ok {
			t.Fatal(err)
		}
	}

	var fired []string
	h.SetOnDelete(func(id string) { fired = append(fired, id) })

	if err := h.DeleteWorkspace("/proj/A"); err != nil {
		t.Fatal(err)
	}
	sort.Strings(fired)
	want := []string{a1.ID, a2.ID}
	sort.Strings(want)
	if len(fired) != len(want) {
		t.Fatalf("应为 A 工作区的 %d 个会话各触发一次，实际 %v", len(want), fired)
	}
	for i := range want {
		if fired[i] != want[i] {
			t.Errorf("触发集合不对：期望 %v，实际 %v", want, fired)
			break
		}
	}
}

// 钩子为 nil（未装配外置）时删除必须照常工作 —— 钩子是可选能力，不是前提。
func TestHistoryDeleteWithoutHook(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := NewHistory(st)
	sess, err := h.Create("", "无钩子")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Delete(sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Get(sess.ID); ok {
		t.Error("会话应已删除")
	}
}
