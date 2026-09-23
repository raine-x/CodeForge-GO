package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeforge/pkg/llm"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 会话 CRUD 全链路：建 → 写消息 → 读回逐条一致 → 列表计数正确 → 删除级联。
func TestSessionCRUD(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	if err := s.CreateSession("s1", "C:\\ws\\a", "会话一", now); err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "你好"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockToolUse, ID: "t1", Name: "read_file", Input: []byte(`{"path":"a.go"}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			{Type: llm.BlockToolResult, ToolUseID: "t1", Content: "file body"},
		}},
	}
	if err := s.SaveSession(SessionRow{
		ID: "s1", Workspace: "C:\\ws\\a", Title: "会话一", CreatedAt: now, UpdatedAt: now, Messages: msgs,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionMessages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(msgs) {
		t.Fatalf("消息数期望 %d，实际 %d", len(msgs), len(got))
	}
	if got[0].Content[0].Text != "你好" || got[1].Content[0].Name != "read_file" || got[2].Content[0].Content != "file body" {
		t.Errorf("消息回读不一致: %+v", got)
	}

	// 列表：workspace 过滤 + 消息计数
	metas, err := s.ListSessions("C:\\ws\\a", false)
	if err != nil || len(metas) != 1 {
		t.Fatalf("ListSessions 期望 1 条，实际 %d（err=%v）", len(metas), err)
	}
	if metas[0].MessageCount != 3 || metas[0].Title != "会话一" {
		t.Errorf("元信息不一致: %+v", metas[0])
	}
	if other, _ := s.ListSessions("C:\\ws\\other", false); len(other) != 0 {
		t.Errorf("其他 workspace 应无会话，实际 %d 条", len(other))
	}

	// 最近会话
	if id, _ := s.LatestSession("C:\\ws\\a"); id != "s1" {
		t.Errorf("LatestSession 期望 s1，实际 %q", id)
	}

	// 重命名
	if err := s.RenameSession("s1", "改名了"); err != nil {
		t.Fatal(err)
	}
	if row, ok, _ := s.GetSession("s1"); !ok || row.Title != "改名了" {
		t.Errorf("重命名未生效: %+v", row)
	}

	// 删除级联
	if err := s.DeleteSession("s1"); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.SessionMessages("s1"); len(msgs) != 0 {
		t.Errorf("删除后消息应级联清空，实际 %d 条", len(msgs))
	}
}

// 记忆 CRUD。
func TestMemoryCRUD(t *testing.T) {
	s := openTestStore(t)

	id1, err := s.AddMemory("C:\\ws\\a", "用户偏好中文回复")
	if err != nil || id1 == 0 {
		t.Fatalf("AddMemory 失败: %v (id=%d)", err, id1)
	}
	if _, err := s.AddMemory("C:\\ws\\a", "项目用 Go 1.25"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddMemory("C:\\ws\\b", "另一个工作区"); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListMemories("C:\\ws\\a")
	if err != nil || len(list) != 2 {
		t.Fatalf("工作区 a 期望 2 条记忆，实际 %d（err=%v）", len(list), err)
	}
	// 新→旧：id 大的在前
	if list[0].Content != "项目用 Go 1.25" {
		t.Errorf("排序应新在前，实际首条 %q", list[0].Content)
	}

	if err := s.UpdateMemory(id1, "更新后的内容"); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListMemories("C:\\ws\\a")
	if list[1].Content != "更新后的内容" {
		t.Errorf("更新未生效: %q", list[1].Content)
	}

	if err := s.DeleteMemory(id1); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListMemories("C:\\ws\\a")
	if len(list) != 1 {
		t.Fatalf("删除后期望 1 条，实际 %d", len(list))
	}
}

// 归档链路：归档 → 列表过滤 → 恢复 → 工作区整组归档 → 满期自动删除。
func TestArchiveFlow(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()

	for _, id := range []string{"a1", "a2"} {
		if err := s.CreateSession(id, "C:\\ws\\x", id, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession("b1", "C:\\ws\\y", "b1", now); err != nil {
		t.Fatal(err)
	}

	// 归档 a1：未归档列表不再出现，归档列表出现
	if err := s.ArchiveSession("a1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessions("C:\\ws\\x", false); len(list) != 1 || list[0].ID != "a2" {
		t.Errorf("归档后未归档列表应只剩 a2，实际 %v", list)
	}
	if arch, _ := s.ListSessions("", true); len(arch) != 1 || arch[0].ID != "a1" {
		t.Errorf("归档列表应只有 a1，实际 %v", arch)
	}

	// 恢复
	if err := s.UnarchiveSession("a1"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessions("C:\\ws\\x", false); len(list) != 2 {
		t.Errorf("恢复后应有 2 条，实际 %d", len(list))
	}

	// 工作区整组归档
	if err := s.ArchiveWorkspace("C:\\ws\\x"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessions("C:\\ws\\x", false); len(list) != 0 {
		t.Errorf("整组归档后该工作区应无未归档会话，实际 %d", len(list))
	}
	if arch, _ := s.ListSessions("", true); len(arch) != 2 {
		t.Errorf("归档列表应有 2 条，实际 %d", len(arch))
	}

	// 整组恢复（与整组归档对称）：侧栏一次点掉整组，恢复也得能一次点回来
	if err := s.UnarchiveWorkspace("C:\\ws\\x"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListSessions("C:\\ws\\x", false); len(list) != 2 {
		t.Errorf("整组恢复后应回到 2 条未归档，实际 %d", len(list))
	}
	if arch, _ := s.ListSessions("", true); len(arch) != 0 {
		t.Errorf("整组恢复后归档列表应为空，实际 %d", len(arch))
	}
	// 幂等：对没有已归档会话的组调用也不该报错
	if err := s.UnarchiveWorkspace("C:\\ws\\y"); err != nil {
		t.Errorf("对无归档会话的组恢复不应报错: %v", err)
	}
	// 再归档回去，供下面的「满期删除」继续用
	if err := s.ArchiveWorkspace("C:\\ws\\x"); err != nil {
		t.Fatal(err)
	}

	// 满期删除（直接 UPDATE 把归档时间改到 11 天前模拟）
	if _, err := s.db.Exec(`UPDATE sessions SET archived_at = ? WHERE archived_at > 0`,
		now.AddDate(0, 0, -11).Unix()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DeleteArchivedOlderThan(10); err != nil || n != 2 {
		t.Fatalf("满 10 天应删除 2 条，实际 %d（err=%v）", n, err)
	}
	if arch, _ := s.ListSessions("", true); len(arch) != 0 {
		t.Errorf("删除后归档列表应为空，实际 %d", len(arch))
	}

	// 工作区列表：y 组仍在
	if wss, _ := s.ListWorkspaces(); len(wss) != 1 || wss[0] != "C:\\ws\\y" {
		t.Errorf("工作区列表应只剩 y，实际 %v", wss)
	}

	// 项目「重命名」= 只改显示名：workspace 键（= 磁盘工作区路径）必须原样不动。
	// 这里守护的正是历史事故：改键会把 C:\...\Desktop\test 抹成 test，项目随即失去工作目录。
	if err := s.SetWorkspaceName("C:\\ws\\y", "我的项目"); err != nil {
		t.Fatal(err)
	}
	if name, _ := s.WorkspaceName("C:\\ws\\y"); name != "我的项目" {
		t.Errorf("显示名应为「我的项目」，实际 %q", name)
	}
	list, _ := s.ListSessions("C:\\ws\\y", false)
	if len(list) != 1 {
		t.Fatalf("改显示名后会话仍应挂在原键下，实际 %d 条", len(list))
	}
	if list[0].Workspace != "C:\\ws\\y" {
		t.Errorf("⚠️ 重命名不得改写工作区键（否则项目失去工作目录），实际 %q", list[0].Workspace)
	}
	if list[0].WorkspaceName != "我的项目" {
		t.Errorf("会话元信息应带上项目显示名，实际 %q", list[0].WorkspaceName)
	}
	// 清空显示名 = 清除自定义名，前端回落按路径末段显示
	if err := s.SetWorkspaceName("C:\\ws\\y", ""); err != nil {
		t.Fatal(err)
	}
	if name, _ := s.WorkspaceName("C:\\ws\\y"); name != "" {
		t.Errorf("清空后显示名应为空串，实际 %q", name)
	}

	// 删除项目要连带清掉显示名记录（否则同名项目重建后会“继承”旧名字）
	if err := s.SetWorkspaceName("C:\\ws\\y", "待删项目"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteWorkspace("C:\\ws\\y"); err != nil {
		t.Fatal(err)
	}
	if name, _ := s.WorkspaceName("C:\\ws\\y"); name != "" {
		t.Errorf("删除项目后显示名记录应一并清掉，实际 %q", name)
	}
}

// 旧 JSON 会话迁移：导入成功、原文件改名 .imported、幂等重跑不重复。
func TestMigrateSessions(t *testing.T) {
	s := openTestStore(t)
	legacyDir := filepath.Join(t.TempDir(), "ws", ".codeforge")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 旧格式：RFC3339 时间字符串（agent.Session 的 json 序列化格式）
	old := `{
	  "id": "legacy1",
	  "title": "旧会话",
	  "created_at": "2026-09-01T10:00:00Z",
	  "updated_at": "2026-09-01T11:00:00Z",
	  "messages": [
	    {"role": "user", "content": [{"type": "text", "text": "在吗"}]},
	    {"role": "assistant", "content": [{"type": "text", "text": "在的"}]}
	  ]
	}`
	if err := os.WriteFile(filepath.Join(legacyDir, "legacy1.json"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	// 一个空 id 文件（旧版启动建会话失败的产物）应被跳过
	if err := os.WriteFile(filepath.Join(legacyDir, "empty.json"), []byte(`{"id":"","messages":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if n := s.MigrateSessions(legacyDir); n != 1 {
		t.Fatalf("期望导入 1 条，实际 %d", n)
	}

	// 导入内容正确
	msgs, err := s.SessionMessages("legacy1")
	if err != nil || len(msgs) != 2 || msgs[0].Content[0].Text != "在吗" {
		t.Fatalf("导入消息不一致: %+v (err=%v)", msgs, err)
	}
	metas, _ := s.ListSessions(filepath.Dir(legacyDir), false)
	if len(metas) != 1 || metas[0].Title != "旧会话" || metas[0].MessageCount != 2 {
		t.Fatalf("workspace=%s 列表不一致: %+v", filepath.Dir(legacyDir), metas)
	}

	// 原文件已标记
	if _, err := os.Stat(filepath.Join(legacyDir, "legacy1.json.imported")); err != nil {
		t.Fatal("导入后原文件应改名 .json.imported")
	}

	// 幂等：重跑不再导入
	if n := s.MigrateSessions(legacyDir); n != 0 {
		t.Fatalf("重复迁移应导入 0 条，实际 %d", n)
	}
}

// 消息来源标记（Origin）必须落盘：不存的话重启后分不清哪句是提问、
// 哪句是运行中转向注入的插话 —— 界面回放会把插话渲染成普通用户消息，
// 「重新生成 / 编辑重发」也可能定位到插话上。
func TestMessageOriginRoundTrip(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	if err := s.CreateSession("s1", "", "来源", now); err != nil {
		t.Fatal(err)
	}
	msgs := []llm.Message{
		llm.TextMessage(llm.RoleUser, "重构 X"),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{{Type: llm.BlockText, Text: "顺便改 b"}}, Origin: "steer"},
	}
	if err := s.SaveSession(SessionRow{
		ID: "s1", Title: "来源", CreatedAt: now, UpdatedAt: now, Messages: msgs,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionMessages("s1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("消息数期望 2，实际 %d", len(got))
	}
	if got[0].Origin != "" {
		t.Errorf("正常提问不应带来源标记，实际 %q", got[0].Origin)
	}
	if got[1].Origin != "steer" {
		t.Errorf("插话的来源标记必须回读一致，实际 %q", got[1].Origin)
	}
}
