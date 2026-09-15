# 开发文档：持久化存储（对话 / 记忆 / 技能）

本文档描述 CodeForge-Go 的持久化体系：SQLite 会话存储（含旧数据迁移）、
用户记忆、项目级记忆（AGENTS.md / CLAUDE.md）、技能（SKILL.md）。

## 一、总体架构

```
cmd/agent/main.go ── store.Open(用户级库) ──┬─ agent.History（会话 CRUD）
                                           ├─ agent.SetMemoryStore（用户记忆）
                                           └─ MigrateSessions（旧 JSON 迁移）
pkg/store/          SQLite 存储层（驱动：modernc.org/sqlite，纯 Go 零 CGO）
pkg/agent/history.go    会话管理（对外 API，内部走 store）
pkg/agent/memory.go     用户记忆（注入 System Prompt）
pkg/agent/project_memory.go  AGENTS.md / CLAUDE.md 扫描注入
pkg/agent/skills.go     SKILL.md 解析、触发词匹配、注入
pkg/tools/builtin/memory_tool.go  save_memory 工具
```

## 二、SQLite 存储（pkg/store）

**库文件**：`%USERPROFILE%/.codeforge/data.db`（用户级，跨工作区共享）。
`store.DefaultPath()` 解析；`os.UserHomeDir()` 失败回退相对路径。
WAL 模式 + `busy_timeout(5000)` + `MaxOpenConns(1)`（单文件库限制写并发）。

**Schema**（`store.go` migrate，幂等建表）：

```sql
sessions(id TEXT PK, workspace TEXT, title TEXT, created_at INT, updated_at INT)
  -- 索引 idx_sessions_ws(workspace, updated_at DESC)
messages(session_id, seq, role, content, PK(session_id, seq), FK→sessions ON DELETE CASCADE)
memories(id INTEGER PK AUTOINCREMENT, workspace, content, created_at, updated_at)
  -- 索引 idx_memories_ws(workspace)
```

- `messages.content` 存 `[]llm.ContentBlock` 的 JSON，与内存结构零转换；
- 写会话 = 事务内「元信息 UPDATE + 消息全删全写」（简单幂等）；删除会话级联删消息。

**workspace 隔离语义**：所有数据带 `workspace` 列（= Agent 当前 `workDir` 绝对路径）。
- 会话列表/记忆列表按当前工作区过滤；
- 未选工作区时 workspace 为空串，数据同样隔离（空工作区自成一组）；
- 切换工作区（前端选择器 → `/api/workspace` → `agent.SetWorkDir`）即切换数据视图。

### 旧 JSON 迁移（migrate.go）

启动时 `st.MigrateSessions(<旧DataDir>)` 扫描 `.codeforge/*.json`：

- 兼容旧格式（RFC3339 时间字符串）逐条导入；
- 导入成功后原文件改名 `<id>.json.imported` **保留不删**；
- 幂等：`.imported` 标记或同 id 已在库 → 跳过；
- workspace 回填为旧 .codeforge 目录的父目录（旧数据所在工作区）。

## 三、会话持久化与恢复

- `agent.History` 对外 API：`Create(workspace, title)` / `Get(id)` / `Save(id)` /
  `List(workspace)` / `Latest(workspace)` / `Rename` / `Delete`。
- REST：`GET /api/sessions`（当前工作区列表）、`GET /api/sessions?id=`（单会话含全部消息）、
  `POST`（新建）、`PATCH {"id","title"}`（重命名）、`DELETE ?id=`。
- WS：`load_session {session_id}` → 服务端下发 `history` 事件
  `{session_id, title, messages:[{role, content:[{type,text}|{type:tool_use,...}|...]}]}`；
  前端 `replayHistory` 用现有渲染原语（addUser/appendText/addTool/addActions）重建聊天列。
- **刷新/重启自动恢复**：WS `ready` 事件带当前工作区会话列表（更新时间倒序），
  前端无 sessionID 时自动 `load_session` 列表第一条。
- 续聊：历史会话上直接发 `user_message`（带 session_id），消息追加到原会话上下文。

## 四、用户记忆（memory.go + memory_tool.go）

- **写入路径 1（对话）**：用户说「记住 X」→ 模型调用 `save_memory` 工具（content=简洁事实）→
  `agent.AddMemory` → `store.AddMemory(workDir, content)`。
- **写入路径 2（设置页）**：`POST /api/memory {"content"}`；编辑 PATCH、删除 DELETE。
- **注入**：`systemPrompt()` 每轮拼装 `## 用户记忆` 段（当前工作区全部条目）；无记忆整段省略。
- 隔离：记忆按工作区独立（`ListMemories(workDir)`）。

## 五、项目级记忆（project_memory.go）

- 每轮 `systemPrompt()` 扫描工作区根的 `AGENTS.md`、`CLAUDE.md`（都存在则全部注入，AGENTS 在前）；
- 全文注入 `## 项目说明` 段；文件不存在/为空/未选工作区 → 整段省略；
- 每轮重读（项目文件可随时被用户改动），不缓存。

## 六、技能系统（skills.go）

**目录约定**：`<workspace>/.codeforge/skills/<名称>/SKILL.md`

**frontmatter 规范**：

```markdown
---
name: code-review          # 技能名（缺省用目录名）
description: 按项目规范审查代码
triggers: 审查, code review # 触发词，逗号分隔，子串匹配、忽略大小写
enabled: true              # false 则不注入（缺省 true）
---
（正文 = 注入给模型的指令）
```

**注入策略**（`systemPrompt()` 每轮）：

1. **索引段**：全部启用技能的「名称：描述（触发词）」常驻注入（让模型知道可 @提及）；
2. **命中注入**：本轮用户输入含 `@技能名` 或任一触发词 → 该技能正文以 `## 技能：<名>` 注入。

**管理**：`GET /api/skills`（只读列表）；技能以文件为准，直接编辑 SKILL.md 增删改。

## 七、System Prompt 拼装顺序

```
基础提示词（内置或 agent.system_prompt_file）
  └─ ## 用户记忆        （当前工作区记忆，无则省略）
  └─ ## 项目说明        （AGENTS.md + CLAUDE.md，无则省略）
  └─ ## 可用技能        （技能索引，无则省略）
  └─ ## 技能：<名>      （命中的技能正文，0..n 段）
```

基础提示词（`pkg/agent/prompt.go` 的 `DefaultSystemPrompt`）只写行为判据，不复述工具参数；
工具名、HITL 权限、工作区围栏与密钥边界必须与实现保持一致。常规能力（读写 / 搜索 / 验证 /
安全边界）写在基础段，扩展能力（技能、子智能体、插件）一律只声明「以下方注入段为准」，
避免能力关闭时误导模型。基础段有体积上限（`prompt_test.go` 断言 ≤ 4096 字符）——
它每一轮都注入，膨胀会直接吃上下文预算。`agent.system_prompt_file` 是**整体替换**基础段，
上述注入段仍会照常追加。

## 八、测试与验证

- `pkg/store/store_test.go`：会话 CRUD（含消息回读/级联删除）、记忆 CRUD、旧 JSON 迁移幂等；
- `pkg/agent/memory_skills_test.go`：记忆段格式、项目说明注入、SKILL 解析/触发/enabled 过滤；
- 既有测试（server/llm/security/config）全量回归。

## 九、升级注意

1. 首次启动新版本会自动建库并迁移旧 `.codeforge/*.json` 会话（日志会打印迁移条数）；
2. 旧 `data_dir` 配置仅用于迁移来源，新数据一律进 SQLite；
3. 前端为 embed 静态资源，改前端后需重新编译二进制。
