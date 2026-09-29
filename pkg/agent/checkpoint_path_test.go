package agent

import (
	"path/filepath"
	"testing"

	"codeforge/pkg/platform"
	"codeforge/pkg/store"
)

// 2026-09-27 审计：历史回放的工具卡片**永远查不到 diff**。
//
// 根因是两侧路径口径不同：
//   - 前端 `toolFilePath`（ui.js）返回**模型传入的原始路径**，历史里存的也是
//     原始 `Input`（agent.go 的 `Input: tc.Input`）；
//   - 检查点存的是 `FS.Resolve` 之后的**绝对路径**（file_ops.go → snapshot）。
//
// 工具 schema 明确允许相对路径（file_ops.go：「相对工作区或绝对路径」），
// 而旧实现是精确字符串比较 —— 于是 `pkg/x.go`、`./pkg/x.go`、
// Windows 下大小写不同的写法全都匹配不上，点开只剩一句
// 「可能已被回退」的假话（那条改动其实好好地记着）。

// newCpAgent 建一个带指定检查点记录的 Agent，返回可直接用于查询的会话 ID
// （checkpoints 有指向 sessions 的外键，且会话 ID 是随机生成的，不能写死）。
func newCpAgent(t *testing.T, root string, rows []store.CheckpointRow) (*Agent, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := newEmitTestAgentWithStore(t, st)
	// ⚠️ 必须设 memoryStore：CheckpointFor 走它读检查点，没设就直接返回 false
	// （与 server_test.go 曾漏掉 SetMemoryStore 是同一类脚手架缺口）。
	a.SetMemoryStore(st)
	a.SetWorkDir(root)
	sess, err := a.History().Create(root, "cp")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := st.InsertCheckpoint(sess.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	return a, sess.ID
}

func cpRow(path, old string, step int) store.CheckpointRow {
	return store.CheckpointRow{Step: step, Path: path, Existed: true, OldContent: old}
}

// TestCheckpointForToleratesRelativePath 相对路径与 ./ 前缀都要能查到。
func TestCheckpointForToleratesRelativePath(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "pkg", "server", "x.go")
	a, sid := newCpAgent(t, root, []store.CheckpointRow{cpRow(abs, "旧内容", 1)})

	for _, q := range []string{
		abs,                 // 精确
		"pkg/server/x.go",   // 相对
		"./pkg/server/x.go", // ./ 前缀
		"pkg\\server\\x.go", // Windows 分隔符
		filepath.Join("pkg", "..", "pkg", "server", "x.go"), // 带 .. 的相对
	} {
		got, ok := a.CheckpointFor(sid, q, -1)
		if !ok {
			t.Errorf("查询 %q 应命中 %q", q, abs)
			continue
		}
		if got.Path != abs {
			t.Errorf("查询 %q 返回了 %q，期望 %q", q, got.Path, abs)
		}
		if got.OldContent != "旧内容" {
			t.Errorf("查询 %q 的内容不对：%q", q, got.OldContent)
		}
	}
}

// TestCheckpointForCaseInsensitiveOnWindows 大小写差异要能命中。
//
// ⚠️ 这条是**平台相关**的：第 ③ 级的大小写宽容现在只在 Windows 开启。
// Linux / Termux 上 `Pkg/` 与 `pkg/` 是两个不同的目录，忽略大小写会给出
// 另一个文件的 diff —— 那边由 TestCheckpointForCaseSensitiveOnLinux 覆盖。
func TestCheckpointForCaseInsensitiveOnWindows(t *testing.T) {
	if platform.OSName() != "windows" {
		t.Skip("仅 Windows 上文件系统不区分大小写")
	}
	root := t.TempDir()
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(filepath.Join(root, "Pkg", "Server", "X.go"), "旧", 1),
	})
	got, ok := a.CheckpointFor(sid, filepath.Join(root, "pkg", "server", "x.go"), -1)
	if !ok {
		t.Fatal("仅大小写不同也应命中")
	}
	if got.OldContent != "旧" {
		t.Errorf("内容不对：%q", got.OldContent)
	}
}

// TestCheckpointForRespectsSeparatorBoundary 后缀匹配必须落在分隔符边界上：
// partA 不能误配 partA2（与 scopesOverlap 同一口径）。
func TestCheckpointForRespectsSeparatorBoundary(t *testing.T) {
	root := t.TempDir()
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(filepath.Join(root, "sub", "partA2", "f.go"), "别的文件", 1),
	})
	if _, ok := a.CheckpointFor(sid, filepath.Join(root, "sub", "partA", "f.go"), -1); ok {
		t.Error("partA 不应误配 partA2（必须落在分隔符边界上）")
	}
}

// TestCheckpointForRejectsAmbiguousSuffix 同一后缀命中两个不同文件时报未匹配，
// 宁可不给也不给一份张冠李戴的 diff。
func TestCheckpointForRejectsAmbiguousSuffix(t *testing.T) {
	root := t.TempDir()
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(filepath.Join(root, "x", "y", "f.go"), "A", 1),
		cpRow(filepath.Join(root, "z", "y", "f.go"), "B", 2),
	})
	// 精确路径仍应命中
	if _, ok := a.CheckpointFor(sid, filepath.Join(root, "x", "y", "f.go"), -1); !ok {
		t.Error("精确路径必须命中，不能被后缀歧义拖累")
	}
	// 只给后缀 y/f.go → 命中两个不同文件 → 判为未匹配
	if _, ok := a.CheckpointFor(sid, filepath.Join(root, "y", "f.go"), -1); ok {
		t.Error("后缀有歧义时不应返回任一文件（会给用户一份错 diff）")
	}
}

// TestCheckpointForStillRefusesUnrecordedPath 安全红线：容错**只在该会话已记录的
// 路径里找**，绝不能变成任意路径读取。
func TestCheckpointForStillRefusesUnrecordedPath(t *testing.T) {
	root := t.TempDir()
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(filepath.Join(root, "pkg", "x.go"), "旧", 1),
	})
	for _, q := range []string{
		filepath.Join(root, "etc", "passwd"), // 绝对但没被改过
		"../../../etc/passwd",                // 往上逃
		"/etc/passwd",
		"pkg/../../secret.txt", // 相对 + 穿越
		"",                     // 空
	} {
		if _, ok := a.CheckpointFor(sid, q, -1); ok {
			t.Errorf("未记录的路径 %q 必须查不到（容错不得放宽安全边界）", q)
		}
	}
	// 另一个会话的记录也查不到
	if _, ok := a.CheckpointFor("other-session", filepath.Join(root, "pkg", "x.go"), -1); ok {
		t.Error("不得跨会话匹配检查点")
	}
}

// TestCheckpointForStepStillExact 指定 step 时必须精确到那一步。
func TestCheckpointForStepStillExact(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "f.go")
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(p, "第一步之前", 1),
		cpRow(p, "第二步之前", 2),
	})
	if got, ok := a.CheckpointFor(sid, p, 2); !ok || got.OldContent != "第二步之前" {
		t.Errorf("step=2 应取到「第二步之前」，实际 %+v ok=%v", got, ok)
	}
	// 不指定 step：取最早的一条（累计改动的对比基准）
	if got, ok := a.CheckpointFor(sid, p, -1); !ok || got.OldContent != "第一步之前" {
		t.Errorf("未指定 step 应取最早一条，实际 %+v ok=%v", got, ok)
	}
}

// 相对路径 + 指定 step 的组合（真实回放场景就是这个形状）
func TestCheckpointForRelativePathWithStep(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "a", "b.go")
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(abs, "v1", 1),
		cpRow(abs, "v2", 2),
	})
	got, ok := a.CheckpointFor(sid, filepath.Join("a", "b.go"), 1)
	if !ok || got.OldContent != "v1" {
		t.Fatalf("相对路径 + step=1 应命中，实际 %+v ok=%v", got, ok)
	}
}

// 工具调用没产生检查点时（如 read_file）查不到，属预期。
func TestCheckpointForNoRows(t *testing.T) {
	a, sid := newCpAgent(t, t.TempDir(), nil)
	if _, ok := a.CheckpointFor(sid, "任何路径", -1); ok {
		t.Error("没有记录时必须查不到")
	}
}
