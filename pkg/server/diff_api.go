package server

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"codeforge/pkg/tools/builtin"
)

// handleDiff 按需给出「某次文件改动改了什么」的 unified diff。
//
// 为什么需要它：diff 文本只在 tool_call 事件里下发过一次，**不随历史持久化** ——
// 刷新页面 / 切会话后回放出来的工具卡片手上没有 diff。这里用检查点里存的
// 「改动前内容」与当前文件对比，按需补出 diff。
//
// 对比基准会随响应一起返回（compare=current）：历史回放场景拿到的是
// 「改动前 → 现在」的**累计**改动，不是当时那一次编辑的精确 diff，
// 前端据此在面板上注明，避免把两者混为一谈。
//
// 只读接口：不写文件、不改历史。路径必须在会话的检查点里查得到 ——
// 于是它只能读「本次会话确实改过」的文件，不会变成任意路径读取。
func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	sessionID := strings.TrimSpace(q.Get("session_id"))
	path := strings.TrimSpace(q.Get("path"))
	if sessionID == "" || path == "" {
		writeErr(w, http.StatusBadRequest, "查看改动", fmt.Errorf("缺少 session_id 或 path"))
		return
	}
	// step 可选：给了就精确到那一步，没给就取该路径最早的一条（累计改动）。
	step := -1
	if raw := strings.TrimSpace(q.Get("step")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			step = n
		}
	}

	cp, ok := s.agent.CheckpointFor(sessionID, path, step)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"path": path,
			"diff": "",
			"note": "这条改动没有可用的记录（可能已被回退，或不属于本会话）",
		})
		return
	}

	// 改动前的内容来自检查点；「现在」的内容直接读盘。
	old := cp.OldContent
	data, readErr := os.ReadFile(cp.Path)
	if readErr != nil && cp.Existed {
		// 改动前存在、现在读不到（被删/被移走）：如实说明，别把 diff 算成「整份删除」。
		writeJSON(w, http.StatusOK, map[string]any{
			"path": path,
			"diff": "",
			"note": "当前文件读不到（可能已被删除或移动），无法对比",
		})
		return
	}
	cur := string(data)

	added, removed := builtin.LineChurn(old, cur)
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    cp.Path,
		"step":    cp.Step,
		"existed": cp.Existed, // false = 这一步是新建文件
		"diff":    builtin.UnifiedDiff(cp.Path, cp.Path, old, cur),
		"added":   added,
		"removed": removed,
		// compare=current：与当前文件对比（可能是累计改动，非单次编辑）
		"compare": "current",
	})
}
