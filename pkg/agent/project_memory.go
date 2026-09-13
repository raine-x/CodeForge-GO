// project_memory.go 实现项目级记忆：自动读取工作区根的 AGENTS.md / CLAUDE.md，
// 存在即全文注入 System Prompt 的「项目说明」段。
//
// 每轮 runLoop 都重新读文件（项目说明可能被用户随时改动），不做缓存。
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectDocNames 是工作区根下自动识别的项目说明文件（按优先级）。
var projectDocNames = []string{"AGENTS.md", "CLAUDE.md"}

// projectMemorySection 扫描工作区根的项目说明文件，生成「项目说明」注入段。
// 两个文件都存在时全部注入（AGENTS.md 在前）；都没有返回空串。
// workDir 为空（未选工作区）时不扫描。
func (a *Agent) projectMemorySection() string {
	if a.workDir == "" {
		return ""
	}
	var parts []string
	for _, name := range projectDocNames {
		data, err := os.ReadFile(filepath.Join(a.workDir, name))
		if err != nil || strings.TrimSpace(string(data)) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("### %s\n%s", name, strings.TrimSpace(string(data))))
	}
	if len(parts) == 0 {
		return ""
	}
	return "## 项目说明\n以下是当前工作区的项目说明文件，遵循其中的约定：\n" + strings.Join(parts, "\n\n")
}
