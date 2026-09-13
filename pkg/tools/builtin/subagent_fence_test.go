package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"codeforge/pkg/tools"
)

// newFenceFS 造一个带子任务目录结构的工作区夹具。
// 工作区内含 partA/ 与 partB/ 两个互不重叠的子任务范围。
func newFenceFS(t *testing.T) (fs *FS, root string) {
	t.Helper()
	root = t.TempDir()
	for _, d := range []string{"partA", "partB"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("建目录失败: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "partA", "a.go"), []byte("package a"), 0o644); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "partB", "b.go"), []byte("package b"), 0o644); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}
	return NewFS(root), root
}

// fenceCtx 构造一个只允许 partA 的子智能体 context。
func fenceCtx() context.Context {
	return tools.WithSubagentScope(context.Background(), tools.SubagentScope{
		Allowed: []string{"partA"},
		Mode:    "implement",
	})
}

// TestSubagentFenceAllowsDeclaredScope 围栏范围内（含子树）的路径必须放行。
func TestSubagentFenceAllowsDeclaredScope(t *testing.T) {
	fs, _ := newFenceFS(t)
	ctx := fenceCtx()

	for _, p := range []string{"partA/a.go", filepath.Join("partA", "new.go"), "partA"} {
		if _, err := fs.ResolveCheckedCtx(ctx, p); err != nil {
			t.Errorf("围栏内路径 %q 被误拒: %v", p, err)
		}
	}
}

// TestSubagentFenceRejectsOtherScope 围栏外的路径（其它子任务范围）必须拒绝。
func TestSubagentFenceRejectsOtherScope(t *testing.T) {
	fs, _ := newFenceFS(t)
	ctx := fenceCtx()

	for _, p := range []string{"partB/b.go", "partB", "readme.md", filepath.Join("partB", "new.go")} {
		if _, err := fs.ResolveCheckedCtx(ctx, p); err == nil {
			t.Errorf("围栏外路径 %q 未被拒绝", p)
		}
	}
}

// TestSubagentFenceBlocksEscapeOverlaps 越权目标即使形似围栏前缀也必须拒绝
// （partA2 与 partA 不同目录，不可被字符串前缀误匹配）。
func TestSubagentFenceBlocksEscapeOverlaps(t *testing.T) {
	fs, root := newFenceFS(t)
	if err := os.MkdirAll(filepath.Join(root, "partA2"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	ctx := fenceCtx()
	if _, err := fs.ResolveCheckedCtx(ctx, "partA2/x.go"); err == nil {
		t.Error("partA2 不是 partA 子树，围栏必须按路径边界拒绝，不能被字符串前缀骗过")
	}
}

// TestSubagentFenceSurvivesApproval 人工审批放行标记不可穿透子智能体围栏：
// WithScopeApproved 只免工作区越界审批，围栏（子任务互斥）必须仍然生效。
func TestSubagentFenceSurvivesApproval(t *testing.T) {
	fs, _ := newFenceFS(t)
	ctx := tools.WithScopeApproved(fenceCtx())
	if _, err := fs.ResolveCheckedCtx(ctx, "partB/b.go"); err == nil {
		t.Error("审批放行标记不得穿透子智能体围栏——围栏守护子任务互斥，与单次审批语义不同")
	}
}

// TestSubagentFenceNoScopeUnrestricted 无围栏（主智能体或未声明 paths 的只读任务）不受限制。
func TestSubagentFenceNoScopeUnrestricted(t *testing.T) {
	fs, _ := newFenceFS(t)
	for _, p := range []string{"partA/a.go", "partB/b.go"} {
		if _, err := fs.ResolveCheckedCtx(context.Background(), p); err != nil {
			t.Errorf("无围栏时路径 %q 不应被拒: %v", p, err)
		}
	}
}

// TestSubagentFenceAbsoluteAllowedPath 声明绝对路径的围栏同样生效。
func TestSubagentFenceAbsoluteAllowedPath(t *testing.T) {
	fs, root := newFenceFS(t)
	ctx := tools.WithSubagentScope(context.Background(), tools.SubagentScope{
		Allowed: []string{filepath.Join(root, "partA")},
		Mode:    "implement",
	})
	if _, err := fs.ResolveCheckedCtx(ctx, "partA/a.go"); err != nil {
		t.Errorf("绝对路径围栏内被误拒: %v", err)
	}
	if _, err := fs.ResolveCheckedCtx(ctx, "partB/b.go"); err == nil {
		t.Error("绝对路径围栏外未拒绝")
	}
}

// TestSubagentFenceWriteToolRejected write_file 在围栏外的调用必须整体失败且不落盘。
func TestSubagentFenceWriteToolRejected(t *testing.T) {
	fs, root := newFenceFS(t)
	tool := NewWriteFileTool(fs)
	ctx := fenceCtx()

	res, err := tool.Execute(ctx, mustJSON(t, map[string]any{
		"path":    "partB/hack.go",
		"content": "evil",
	}))
	if err != nil || res == nil || res.Success {
		t.Fatalf("围栏外 write_file 必须失败: res=%v err=%v", res, err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "partB", "hack.go")); statErr == nil {
		t.Fatal("围栏外写入必须被拦截，文件不得落盘")
	}
}
