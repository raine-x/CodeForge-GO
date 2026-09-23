// project_memory.go 实现项目级记忆：自动读取工作区根的 AGENTS.md / CLAUDE.md，
// 注入 System Prompt 的「项目说明」段。
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

// projectDocMaxRunes 是单个说明文件的注入上限（字符）。
//
// 必须限幅：项目说明是无外部输入的信任边界，但文件长度由用户/上游仓库决定，
// 一份 200KB 的 AGENTS.md 会直接把「系统提示词 + 工具定义」顶过上下文预算，
// 触发「开销已占满预算」而整个会话无法开始。截断部分由模型按需自行读取全文。
const projectDocMaxRunes = 8000

// projectMemorySection 扫描工作区根的项目说明文件，生成「项目说明」注入段。
// 两个文件都存在时全部注入（AGENTS.md 在前）、内容相同的只注一份；
// 都没有返回空串。workDir 为空（未选工作区）时不扫描。
func (a *Agent) projectMemorySection() string {
	// 取一次快照：判空与拼接路径必须用同一个值（切工作区会改它）。
	workDir := a.WorkDir()
	if workDir == "" {
		return ""
	}
	var parts []string
	var seen []string
	for _, name := range projectDocNames {
		data, err := os.ReadFile(filepath.Join(workDir, name))
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" || containsFold(body, seen) {
			continue // 空文件、或与已注入的文件内容一致（仓库里两份互为副本）
		}
		seen = append(seen, body)
		parts = append(parts, fmt.Sprintf("### %s\n%s", name, clipRunes(body, projectDocMaxRunes)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "## 项目说明\n以下是当前工作区的项目说明文件，遵循其中的约定" +
		"（被截断的部分需要时用读取文件能力取全文）：\n" + strings.Join(parts, "\n\n")
}

// containsFold 判断 body 是否与已注入过的某份内容等价（忽略空白差异）。
func containsFold(body string, seen []string) bool {
	for _, prev := range seen {
		if sameContent(body, prev) {
			return true
		}
	}
	return false
}

func sameContent(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}
