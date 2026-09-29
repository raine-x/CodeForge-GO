# CodeForge-Go · AGENTS.md

> 给所有改这个仓库的 AI 看（Gemini / DeepSeek / GLM / Claude 等），开工前先读完。
> 基线：`master@d9234a0`。行号会漂移，**以函数名、类型名为准**，找不到就 grep。
> 三类内容：**硬规则**（现在必须遵守）、**复用清单**（先找现成的，别重造）、**迁移中**（目标状态，别在旧结构上继续堆）。
> 改变了本文任何规则的改动，必须在同一个 commit 里更新本文件。

---

## 1. 项目速览

- Go 编写的 Agent 框架。目标：跨平台（Windows amd64 / Linux amd64 / Android arm64）、纯静态编译（`CGO_ENABLED=0`）、低内存、浏览器界面。
- 前端：`web/dist/ui.js` 单文件 IIFE，**没有构建步骤**，`go:embed` 内嵌。
- 存储：SQLite（`~/.codeforge/data.db`）；审计日志为 JSONL。

一次对话的调用链：

```
浏览器 ui.js → 传输 pkg/server(WebSocket) → 编排 pkg/agent(ReAct 循环)
  → 执行 pkg/tools Executor → 策略 pkg/security / 内置工具 pkg/tools/builtin
编排 → LLM pkg/llm(SSE)      各处 → 存储 pkg/store
```

---

## 2. 硬规则

### 工具与执行

1. 工具调用**只经 `Executor.Execute`**，它负责策略判定、审批、围栏、审计、截断。不要绕过它直接调工具。
2. 构造 Executor 时 **audit 不许传 nil**。历史：子智能体曾传 nil，操作不进审计且不报错。
3. **每个工具必须实现 `Metadata() Metadata` 声明 `SideEffect`**（none / write / external / destructive）。有逐工具回归测试兜底（`pkg/tools/builtin/side_effect_test.go`，漏声明直接 CI 红）。**副作用等级只能由工具自己声明** —— 历史上有过一张按名字硬编码的只读表，已删：它在两个方向上都错（真只读的工具被误拒；零声明的工具被凭名字放行，而工具名是插件可控的）。未声明或取值非法一律按最严处理（只读模式下拒绝）。
3a. 工具的**其余**可选能力（`ScopeChecker` / `DiffProvider` / `ReadGate` / `TimeoutPolicy`，见 `pkg/tools/tool.go`）目前仍是运行时类型断言，**漏实现是静默降级**，例如漏 `DiffProvider` = 用户盲批。新增写类工具必须逐个核对。`ReadOnlyTool` 已废弃，请改用 3。
3b. 工具的**越界检查、审批 diff** 仍靠上述可选接口发现 —— 这部分尚未有编译期或注册期校验，是已知缺口。
4. 内置工具里**不要新增**直接的 `os.*` / `exec.*` 调用，文件与命令访问走现有抽象点（迁移中，见第 5 节）。平台差异放 `pkg/platform`，`terminal.go` 不直接碰 `syscall`。子进程管理走 `platform.StartGrouped` / `platform.CloseGroup`：Unix 用 `Setpgid` + 负 pgid 发信号，Windows 用 Job Object + `taskkill /T` 补 `AssignProcessToJobObject` 之前的竞态窗口；`cmd.WaitDelay` 必须一起设，否则孙进程攥着管道会让 `cmd.Wait()` 永久阻塞。
5. 文件写入必须走 `FS.casWrite` / `casEdit` / `casRemove`，**不要**自己 `os.WriteFile` 配一次写前的 `requireReadSeen`。前者把「读当前 → 比对 → 写」收进同一把按路径的锁；后者两步之间留有窗口，并发写会静默互相覆盖（两次都"成功"，后写的覆盖先写的，不留任何痕迹）。落盘用 `atomicWriteFile`（同目录临时文件 + rename）—— 直接 `os.WriteFile` 是"打开→截断→写"，进程写一半被杀会留下半截文件。
5a. 写前校验的完整语义：未读过 → 拒；读取后被外部改动（`seenEntry` 存 SHA-256 指纹 + 大小）→ 拒；写入时文件存在性与调用方判断不符 → 拒。写完、编辑完、撤销后都必须刷新指纹，否则连续第二次写会被误判成外部修改而永久锁死。上下文压缩后必须调 `ForgetReads` 清登记。
5b. `delete_file` 同样要先读。删没读过的文件与覆盖它是同一类丢数据，且更不可逆。
6. 工具结果超过 `maxOutput` 时，输出必须仍是合法 JSON，不能在半截 JSON 处硬切。大结果由工具自己分页或限流，Executor 的截断只是兜底。
7. 文件围栏由 `within` + `resolveReal`（逐级 `EvalSymlinks`）+ `checkScope` 组成，专防"区内软链指向区外、且目标尚不存在"的写入绕过。改动这三个函数必须带攻击用例测试。
8. `WithScopeApproved` 只在审批确实放行了越界目标时才注入。历史 bug：被注入到了不该注入的地方。

### 策略与配置

9. `agent.hidden_tools`（Exposure，控制发给 LLM 的工具表）与 `security.Policy`（控制是否放行）是**刻意分离**的：隐藏不等于禁止。不要合并成一个开关。
10. **禁止调用 `Config.SaveWholeConfig()`**（原名 `Save`，已改名并标 `Deprecated`，旧名删除后调用即编译失败）。它整份序列化，yaml 对切片是替换而非合并，会静默吃掉规则。历史：`security.rules` 曾因此丢过 `todo_write`、`web_*`。界面可改项走 `stateProjection` + `SaveState`（`pkg/config/state.go` 头部有纪律说明）。
11. 策略规则的条件解析失败 = 拒绝（fail-closed），不是跳过。

### 审批

11a. 人工审批有**自己**的超时（`DefaultApprovalTimeout` = 15 分钟，`SetApprovalTimeout` 可覆盖），不借用工具执行超时。判定审批超时用 `errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil`，**不要**把超时当成拒绝返回 —— 两者在界面上必须能区分。

### 编排与会话

12. 子智能体相关的策略上限在三处落点生效（Agent / 调度器 / 工具 schema）。改一处必须三处同步，否则会出现"界面显示 2、实际放行 5"。
13. Agent 自己不知道会话 id：`Event.SessionID` 由 WS 层补，会话信息经 `tools.SessionFrom(ctx)` 传递。不要在 Agent 上加会话字段。
14. `Compress` 必须对输入深拷贝，**绝不写回原历史**。历史 bug：浅拷贝导致发一次请求顺手删掉自己的历史。
15. 目标模式：审查者与 `goal_verify` 工具必须**一起**注册；审查者的 audit 传真 logger。
16. 错误用 `%w` 包装，`errs.Classify` 才能生效。按 Kind 驱动重试、压缩、上报，不要在调用点靠字符串匹配。

### 前端

17. 改 `ui.js` 后必须跑 `node web/test/render_md.test.js` 和 `node web/test/attachments.test.js`（`make test-web` 现已同时跑这两个）。测试里有对源码文本的正则断言，改函数名或结构要同步测试。
18. 新增接口请求优先走统一封装（若已有 `api()`），不要再新增裸 `fetch`。

---

## 3. 复用清单（先找现成的）

| 需要 | 用这个 | 位置 |
|---|---|---|
| 错误分类 | `errs.Classify` / `Kind` | `pkg/errs` |
| 选 shell、平台差异 | `platform.Shell()` 等 | `pkg/platform` |
| 取会话信息 | `tools.SessionFrom(ctx)` | `pkg/tools` |
| 截断字符串（不切坏汉字） | `truncateString` | `pkg/tools/executor.go` |
| 审计（nil 接收者安全） | `security.AuditLogger` | `pkg/security` |
| 配置持久化 | `stateProjection` / `SaveState` | `pkg/config/state.go` |
| 把远端能力适配成 `tools.Tool` | `RemoteTool` | `pkg/tools/plugins/loader.go` |
| 撤销 | `Undoer` 接口 | `pkg/server/server.go` |
| 上下文预算与压缩 | `prepareMessagesBudget` / `Compress`（四档） | `pkg/agent` |
| 系统提示与项目记忆 | `prompt.go` / `project_memory.go`（AGENTS.md 优先于 CLAUDE.md） | `pkg/agent` |
| 策略判定 | `Policy.Evaluate`（有序判定链） | `pkg/security/policy.go` |
| 按暴露面裁剪工具表 | `registry.DefinitionsFor` + Exposure | `pkg/tools/registry.go` |

**规则**：新增功能前，先 grep 上表和相邻包。确实要新增，在提交说明里写清"现有的为什么不够"。

---

## 4. 开发流程

- 每个改动单独一个 commit，范围只限任务本身，**不顺手重构**。
- 修 bug：先写会失败的测试，再修，再跑全量。
- 必跑：`gofmt`、`go vet ./...`、`go test ./...`。e2e 默认 Skip，需 `CODEFORGE_E2E=1`。
- 全平台编译检查：`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"`，目标 Windows amd64 / Linux amd64 / Android arm64。
- 纯重构：任何一个原有测试变红立即回滚，不要"顺手修一下"。
- 安全相关（围栏、策略、审批）：测试没全绿不许合入。
- 尽量不新增第三方依赖（静态编译与体积是产品目标），必须新增时说明理由。

---

## 5. 迁移中（目标状态）

目标方向：本地与远程共用同一套工具，工作区归会话而不是进程。

- [ ] 执行环境分层：底层 Backend 只做纯 IO（Read/Write/Stat/List/Remove/Realpath/Grep/Glob/Exec）；上层会话级 Guard 负责越界检查、指纹、快照撤销。"是否在工作区内"的判断只在 Guard 写一份，后端只提供 `Realpath`。
- [ ] 执行器角色工厂（`Role` + `NewExecutorFor`），审计等参数不再靠手工传
- [ ] 每次工具调用一条权威 `ExecutionRecord`，审计、界面事件、检查点都从它派生
- [ ] 工作区归会话：`Workspace` 结构化；`undo` / `readSeen` 移到会话；`Snapshot` 加 `SessionID`
- [ ] 工具声明 `SideEffect`（读 / 写 / 外部 / 破坏性），驱动审批与并发
- [ ] ReAct 循环状态化 + 多 tool_call 批处理
- [ ] 事件日志与序号（低优先级）

**等待中，请勿启动**：Backend/Guard 分层、工作区会话化、循环状态化。在架构对照分析出结论前，不要动这三项，否则很可能返工。

---

## 6. 已知问题（修完请从此列表删除）

- `checkpoints.go` 路径匹配第三级大小写不敏感，Linux 上可能把 `partA` / `partA2` 判为同一文件
- 撤销栈只限条数，不限字节
- `ScopeChecker` / `DiffProvider` / `ReadGate` / `TimeoutPolicy` 仍是运行时断言，漏实现即静默降级（见硬规则 3a）
- CAS 只保证**同一进程内**并发调用互斥，未用 OS 级文件锁防「另一个进程在临界区内改文件」
- 未决审批在**刷新页面**后丢失（服务端 `pending` 挂在旧连接的 `wsApprover` 上，页面刷新即失去渠道；注意这与「审批无上界」已修是两件事）
- 审计日志的 `run.json` 里 token 明文落盘
- `History.cache` 只增不减
