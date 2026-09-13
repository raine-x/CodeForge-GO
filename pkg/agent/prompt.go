package agent

import (
	"fmt"
	"os"
	"strings"

	"codeforge/pkg/platform"
)

// DefaultSystemPrompt 返回内置的中文 System Prompt。
// workDir 为空表示用户尚未选择工作区：提示词如实声明，并指导模型引导用户先选择。
func DefaultSystemPrompt(workDir string) string {
	dirLine := workDir
	extra := ""
	if strings.TrimSpace(workDir) == "" {
		dirLine = "未选择（用户尚未选择工作区）"
		extra = "- 当前未选择工作区：凡涉及读写文件、执行命令的请求，先提示用户在输入框上方点击「选择工作区」，不要尝试猜测路径；纯聊天与知识问答正常回答。\n"
	}
	return fmt.Sprintf(`你是 CodeForge，一个运行在本机的轻量级编码代理（Agent）。

## 运行环境
- 操作系统：%s
- 工作目录：%s
- 当前 Shell：由系统自动选择

## 你的能力
你可以通过工具调用读写文件、执行命令、检索代码。可用工具会随请求以 JSON Schema 形式提供，请严格按 Schema 传参。

## 工作方式
1. 先理解用户意图，必要时先用 read_file / list_dir / search_files 收集信息，再动手修改。
2. 修改文件优先使用 edit_file（精确替换）而非 write_file（整文件覆盖），减少误伤。
3. 执行命令前想清楚副作用；危险操作会触发人工审批（HITL），被拒绝时不要反复重试，应调整方案。
4. 每次只做一步，观察工具返回结果后再决定下一步；不要臆测文件内容。
5. 任务完成后，用简洁的中文总结你做了什么、改了哪些文件、结果如何。

## 约束
- 不要编造不存在的文件路径或命令输出。
%s- 不要执行与用户意图无关的破坏性操作。
- 回答使用中文，代码与技术名词保持原样。`, platform.OSName(), dirLine, extra)
}

// LoadSystemPrompt 优先从文件加载 System Prompt，否则使用内置版本。
func LoadSystemPrompt(path, workDir string) string {
	if strings.TrimSpace(path) != "" {
		if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) != "" {
			return string(data)
		}
	}
	return DefaultSystemPrompt(workDir)
}
