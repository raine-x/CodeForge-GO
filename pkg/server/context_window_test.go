package server

import (
	"testing"

	"codeforge/config"
)

// 模型窗口（ctx_in）驱动压缩阈值的接线测试。
//
// 单独成文件是为了不与 context_usage_test.go 的口径测试混在一起：
// 那边测「数字怎么算」，这里测「阈值的来源」。

// TestSyncContextWindowDrivesCompressBudget 模型库的 ctx_in 必须真正驱动压缩阈值。
//
// 回归：该字段长期「只存不用」—— 前端能填「输入上下文」，模型库也存得下，
// 但压缩线一直写死 agent.context_token_budget，换成小窗口模型时压缩完全不
// 触发，请求直接超窗被上游拒绝。
func TestSyncContextWindowDrivesCompressBudget(t *testing.T) {
	deps := newTestDeps(t)
	deps.cfg.LLM.Model = "glm-test"
	// 输出上限恰好占窗口一半：这是最容易踩的配置
	//（窗口 262144、输出 131072，若按窗口 95% 当输入预算必然超窗）。
	deps.cfg.LLM.MaxTokens = 131072
	deps.agent.SetLLMConfig(deps.cfg.LLM)

	srv := deps.newServer()
	if err := srv.ModelStore().Upsert(config.ModelEntry{ID: "glm-test", CtxIn: 262144}); err != nil {
		t.Fatalf("写入模型库失败: %v", err)
	}
	srv.SyncContextWindow()

	got := srv.contextUsage("")
	if got["window"] != 262144 {
		t.Errorf("window = %v，期望 262144（模型库 ctx_in）", got["window"])
	}
	if got["reserve"] != 131072 {
		t.Errorf("reserve = %v，期望 131072（输出预留 = min(输出上限, 窗口/2)）", got["reserve"])
	}
	// (262144 − 131072) × 0.80 = 104857.6 → 四舍五入 104858
	const wantBudget = 104858
	if got["budget"] != wantBudget {
		t.Errorf("budget = %v，期望 %d", got["budget"], wantBudget)
	}
	// 输入预算 + 输出预留不得超过窗口，否则压缩线本身就保证了超窗。
	if budget, _ := got["budget"].(int); budget+131072 > 262144 {
		t.Errorf("budget(%d) + 输出预留(131072) > 窗口 262144，压缩线失效", budget)
	}
}

// TestSyncContextWindowFallsBackWithoutModel 模型不在库中（或未填 ctx_in）时，
// 压缩线回退到绝对阈值，绝不能变成 0 —— 那会让每一轮都判定超预算。
func TestSyncContextWindowFallsBackWithoutModel(t *testing.T) {
	deps := newTestDeps(t)
	srv := deps.newServer()
	srv.SyncContextWindow() // 模型库中不存在当前模型

	got := srv.contextUsage("")
	if got["window"] != 0 {
		t.Errorf("window = %v，期望 0（未配置）", got["window"])
	}
	if got["budget"] != 120000 {
		t.Errorf("budget = %v，期望 120000（窗口未知时回退 config 值）", got["budget"])
	}
	if got["reserve"] != 0 {
		t.Errorf("reserve = %v，窗口未知时不应有预留", got["reserve"])
	}
}

// TestSyncContextWindowZeroCtxIn 模型在库中但没填「输入上下文」时同样回退，
// 不能把 0 当成窗口（0 × 0.95 = 0）。
func TestSyncContextWindowZeroCtxIn(t *testing.T) {
	deps := newTestDeps(t)
	deps.cfg.LLM.Model = "no-window"
	srv := deps.newServer()
	if err := srv.ModelStore().Upsert(config.ModelEntry{ID: "no-window"}); err != nil {
		t.Fatalf("写入模型库失败: %v", err)
	}
	srv.SyncContextWindow()

	got := srv.contextUsage("")
	if got["window"] != 0 || got["budget"] != 120000 {
		t.Errorf("ctx_in 缺省时应回退：window=%v budget=%v", got["window"], got["budget"])
	}
}
