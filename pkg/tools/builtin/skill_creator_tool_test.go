package builtin

import (
	"codeforge/pkg/tools"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// create_skill：合法调用写入 SKILL.md（frontmatter + 正文）；非法名拒绝；同名覆盖。
func TestSkillCreatorTool(t *testing.T) {
	dir := t.TempDir()
	tool := NewSkillCreatorTool(NewFS(dir))
	ctx := context.Background()

	call := func(args map[string]any) *tools.ToolResult {
		raw, _ := json.Marshal(args)
		res, _ := tool.Execute(ctx, raw)
		return res
	}

	// 正常创建
	res := call(map[string]any{
		"name":        "deploy",
		"description": "部署到测试环境",
		"triggers":    "部署, deploy",
		"content":     "先跑测试再部署。",
	})
	if res == nil || !res.Success {
		t.Fatalf("创建技能失败: %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".codeforge", "skills", "deploy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"name: deploy", "description: 部署到测试环境", "triggers: 部署, deploy", "enabled: true", "先跑测试再部署"} {
		if !strings.Contains(text, want) {
			t.Errorf("SKILL.md 缺少 %q：\n%s", want, text)
		}
	}

	// 同名覆盖（迭代）
	res = call(map[string]any{"name": "deploy", "description": "v2", "content": "新步骤"})
	if res == nil || !res.Success || !strings.Contains(res.Data.(string), "已更新") {
		t.Fatalf("迭代已有技能应提示已更新: %+v", res)
	}

	// 非法名（路径穿越 / 大写）拒绝
	for _, bad := range []string{"../evil", "BadName", "", "a b"} {
		if res := call(map[string]any{"name": bad, "content": "x"}); res != nil && res.Success {
			t.Errorf("非法技能名 %q 应被拒绝", bad)
		}
	}

	// 空 content 拒绝
	if res := call(map[string]any{"name": "ok1"}); res != nil && res.Success {
		t.Error("空 content 应被拒绝")
	}
}
