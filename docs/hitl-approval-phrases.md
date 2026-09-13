# 开发文档：HITL 审批文案的中文短语映射

## 背景

HITL（Human-in-the-Loop）审批条此前直接拼接原始数据：

```
需要审批：write_file（hello.html）
需要审批：run_command（start "" "C:\Users\26536\Desktop\test\hello.html"）
```

存在问题：

1. 暴露内部工具 id（`write_file` / `run_command`）；
2. 把动作参数明文（完整命令、绝对路径）直接展示在对话流里，冗长且干扰阅读。

现改为与工具卡片（`toolLabel`）一致的中文短语形式，审批条只显示短语，不暴露参数明文：

```
需要审批：写入文件
需要审批：执行命令
```

## 实现位置

全部改动在前端 `web/dist/ui.js`：

- `toolPhrases` 常量：7 个内置工具 → 中文短语映射表；
- `toolPhrase(name)`：查表函数，未识别的工具（如插件工具）回退为 `调用 <原始工具名>`，不会显示空白；
- `addApproval(req)`：审批条渲染。文案改为 `需要审批：` + 短语；判定理由（`req.reason`）与动作目标（`req.action`）合并放进 `title` 悬浮提示，需要时悬停可查看，不再出现在可见文本里。

## 映射表

| 工具 id         | 中文短语 |
|-----------------|----------|
| `read_file`     | 读取文件 |
| `list_dir`      | 浏览目录 |
| `search_files`  | 搜索文件 |
| `run_command`   | 执行命令 |
| `write_file`    | 写入文件 |
| `edit_file`     | 编辑文件 |
| `delete_file`   | 删除文件 |

## 数据来源（后端）

审批请求由服务端经 WebSocket 推送，消息类型 `hitl_request`，字段：

- `approval_id`：审批单号，前端回传 `hitl_decision` 时使用；
- `tool`：工具 id（即上表的 key）；
- `action`：语义动作（命令 / 路径），现仅进入悬浮提示；
- `reason`：策略判定理由（如「命中危险操作黑名单」）。

相关代码：`pkg/tools/executor.go`（`ApprovalRequest` 构造）、`pkg/server/ws_handler.go`（事件推送）。

## 扩展指引

- **新增内置工具**：在 `web/dist/ui.js` 的 `toolPhrases` 中补一行映射，否则审批条会显示 `调用 <工具名>`；
- **插件工具**：无法预知名字，依赖回退分支，无需改动；
- **修改文案**：只改 `toolPhrases`，工具卡片 `toolLabel` 与审批条 `addApproval` 各自独立成句，互不影响。

## 附：动态权限模式与配置规则的优先级

权限模式（`/api/perm`，readonly/ask/auto）与 `config/default.yaml` 中 `security.rules` 的关系：

| 模式         | 行为                                                                 |
|--------------|----------------------------------------------------------------------|
| `ask` 请求   | 按配置规则与 `default_decision` 执行（原始行为）                     |
| `auto` 自主  | 配置规则命中的 **Ask 升级为 Allow**；Deny 规则、黑名单、插件强制审批仍生效 |
| `readonly` 只读 | **跳过配置规则**，默认 Deny，仅只读工具放行，防止 Allow/Ask 规则绕过锁定 |

规则匹配优先级高于默认判定（`pkg/security/policy.go` 的 `Evaluate`），因此把写类工具硬编码为
`decision: "ask"` 的规则会拦截「自主」模式——这正是 `auto` 模式下 Ask→Allow 升级存在的原因。
实现见 `pkg/security/policy.go`（`SetMode` / `Evaluate`），测试见 `pkg/security/policy_test.go`。

## 部署提醒

`web/dist` 通过 `embed` 打进二进制（`web/embed.go`），修改前端后必须重新 `go build` 并重启服务才能生效。
