package agent

import (
	"path/filepath"
	"testing"

	"codeforge/pkg/platform"
	"codeforge/pkg/store"
)

// 检查点路径匹配的第三级：大小写不敏感只在**文件系统本身不区分大小写**的
// 平台上开启。
//
// 改造前第 ③ 级无条件 `strings.ToLower`。在 Linux / Android(Termux) 上
// `Pkg/Server/X.go` 与 `pkg/server/x.go` 是**两个不同的文件**，而折叠后
// 它们相等 —— 于是 /api/diff 会返回另一个文件的 diff。
//
// 更正一处此前的描述：`partA` / `partA2` 其实**触发不了** ——
// 第 ③ 级的 `HasSuffix(lp, "/"+lower)` 已要求落在分隔符边界上，
// 折叠只改字符、不改分隔符，所以 `partA2/f.go` 不会匹配 `partA/f.go`。
// 真正能误判的只有「整体仅大小写不同」，走的是 `lp == lower` 那一项。
//
// 用现成的 newCpAgent / cpRow 夹具（checkpoint_path_test.go 里的那一套）。

// TestCheckpointForCaseSensitiveOnLinux 大小写敏感平台上不得误配。
//
// ⚠️ 这条直接测 pathMatchersCase 的注入参数，**不用平台门** ——
// 两种大小写语义天然互斥，而开发机是 Windows：若只按当前平台生效，
// 「区分大小写时不该匹配」这一侧在本机永远测不到，
// 而那恰恰是这次修的 bug。
func TestCheckpointForCaseSensitiveOnLinux(t *testing.T) {
	root := t.TempDir()
	// 登记的路径里是大写 Pkg
	recorded := filepath.Join(root, "Pkg", "Server", "X.go")
	other := filepath.Join(root, "pkg", "server", "x.go")

	// 区分大小写：不该匹配
	for i, match := range pathMatchersCase(other, root, false) {
		if match(recorded) {
			t.Errorf("第 %d 级误配：区分大小写时 %s 不该匹配 %s", i+1, other, recorded)
		}
	}
	// 不区分大小写：应匹配（守住第 ③ 级在 Windows 上的既有行为）
	hit := false
	for _, match := range pathMatchersCase(other, root, true) {
		if match(recorded) {
			hit = true
		}
	}
	if !hit {
		t.Errorf("不区分大小写时应匹配 %s → %s", other, recorded)
	}
}

// TestCaseInsensitiveOSOnlyWindows 大小写宽容只应在 Windows 开启。
//
// 特别要点名 Termux：它是交叉编译目标（AGENTS.md §1 列为三大平台之一），
// ext4 区分大小写。若这里写成 runtime.GOOS == "windows" 就仍然对，
// 但若有人图省事改成「非 Linux 即不区分大小写」，Termux 会被误判。
func TestCaseInsensitiveOSOnlyWindows(t *testing.T) {
	got := caseInsensitiveOS()
	switch platform.OSName() {
	case "windows":
		if !got {
			t.Error("Windows 应视为不区分大小写")
		}
	default:
		if got {
			t.Errorf("%s 应视为区分大小写", platform.OSName())
		}
	}
}

// TestCheckpointForSuffixStillWorksWithoutRoot 第 ③ 级的**独有**能力不能丢。
//
// 工作区被清空 / 切换时（SetWorkDir("")），第 ② 级因 root 为空被跳过，
// 只有第 ③ 级能靠分隔符归一化找回那张卡片原本对应的文件。
// 去掉第 ③ 级等于让「工作区切换后历史 diff 全部查不到」。
func TestCheckpointForSuffixStillWorksWithoutRoot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "pkg", "sub", "f.go")
	a, sid := newCpAgent(t, root, []store.CheckpointRow{cpRow(target, "旧内容", 1)})

	a.SetWorkDir("") // 第 ② 级失效
	if got, ok := a.CheckpointFor(sid, target, -1); !ok {
		t.Errorf("工作区为空时第 ③ 级应仍能命中，实际查不到 %s", target)
	} else if got.Path != target {
		t.Errorf("命中的应是 %s，实际 %s", target, got.Path)
	}
}

// TestPickCheckpointRejectsAmbiguousSuffixWithStep 带 step 的查询也要查歧义。
//
// 既有实现在 `step >= 0` 时直接 return，从不检查命中了几条 —— 于是
// 「<root>/x/y/f.go」与「<root>/z/y/f.go」都记在 step 1 时，
// 查 y/f.go?step=1 会静默给出其中一条。
//
// 这是同一类「静默给错行」的缺陷。改完第 ③ 级之后两类歧义
// （大小写不同、同后缀不同根）会更容易命中，留着等于修一半。
func TestPickCheckpointRejectsAmbiguousSuffixWithStep(t *testing.T) {
	root := t.TempDir()
	a, sid := newCpAgent(t, root, []store.CheckpointRow{
		cpRow(filepath.Join(root, "x", "y", "f.go"), "A", 1),
		cpRow(filepath.Join(root, "z", "y", "f.go"), "B", 1),
	})

	// 精确路径应各自命中，不受影响
	if _, ok := a.CheckpointFor(sid, filepath.Join(root, "x", "y", "f.go"), 1); !ok {
		t.Error("精确路径应命中")
	}
	// 后缀 + step：命中了两条不同路径 → 必须判为歧义
	ambiguous := filepath.Join(root, "y", "f.go")
	if got, ok := a.CheckpointFor(sid, ambiguous, 1); ok {
		t.Errorf("带 step 的后缀查询命中了多条不同路径，必须判为歧义，实际拿到 %s", got.Path)
	}
}
