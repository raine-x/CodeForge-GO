# 开发文档：内置插件（Builtin Plugins）

内置插件是**进程内、可开关**的增强能力，区别于 MCP 插件（外部进程）：
无需启动子进程，能力以「System Prompt 注入 + 专用工具」的形式提供。

## 工作方式

1. **开关**：`config/default.yaml`（可被 local.yaml 覆盖）：

   ```yaml
   builtin_plugins:
     skill_creator: true   # 缺省开启（未配置 = 开）；写 false 关闭
   ```

2. **启用后**（main.go 接线）：
   - 该插件的专用工具注册进工具注册中心（如 `create_skill`）；
   - Agent 的 System Prompt 注入插件说明段；
3. **AI 动态选择**：模型根据注入的「调用时机」自行判断本轮是否使用；
   一旦决定使用，约定先在回复中说明「使用插件 XXX」再调用工具；
4. **工作流提醒**：前端按工具名把 `create_skill` 渲染为
   「使用插件 Skill Creator：创建技能 xxx」（`web/dist/ui.js` 的 toolLabel / toolPhrases）；
5. 写入类操作走统一安全策略（ask 模式下照样弹审批，审批条显示
   「需要审批：Skill Creator 插件」）。

## 现有内置插件

### Skill Creator（ID: `skill_creator`）

| 项 | 内容 |
|----|------|
| 工具 | `create_skill` |
| 能力 | 创建/更新工作区技能 `<workspace>/.codeforge/skills/<名称>/SKILL.md` |
| 调用时机 | 用户要求「创建一个技能 / 把这套流程保存下来以后复用 / 改进某个技能」；一次性任务不使用 |
| 关闭方式 | `builtin_plugins.skill_creator: false` |

## 如何新增内置插件

1. `pkg/agent/builtin_plugins.go`：在 `builtinPlugins()` 追加 `BuiltinPlugin`
   定义（ID/Name/Purpose/WhenToUse/Instructions），并加对应 `SetXxxEnabled` + 字段；
2. `pkg/tools/builtin/`：实现插件工具（`Name/Description/InputSchema/Execute`）；
3. `cmd/agent/main.go`：配置启用时 `RegisterXxx(registry, ...)` + `ag.SetXxxEnabled(true)`；
4. `config/config.go`：`BuiltinPluginsConfig` 加开关字段（`*bool`，nil=开启）；
5. `web/dist/ui.js`：`toolPhrases` + `toolLabel` 加该插件工具的中文提醒文案；
6. `config/default.yaml`：加注释样例。

## 相关代码

- 定义与注入：`pkg/agent/builtin_plugins.go`（`builtinPluginSection`，systemPrompt 拼装）
- 能力工具：`pkg/tools/builtin/skill_creator_tool.go`
- 配置：`config/config.go`（`BuiltinPluginsConfig`）
- 接线：`cmd/agent/main.go`（「内置插件」注释处）
