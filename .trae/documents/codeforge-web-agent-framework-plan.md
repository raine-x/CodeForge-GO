# CodeForge-Go 跨平台 Web-Agent 框架实施计划

依据《跨平台 Web-Agent 代理框架与可扩展插件引擎综合设计文档》（项目绣球文档.docx）制定。

## 一、文档分析摘要

目标：实现类 Codex / Claude Code 的轻量级可扩展 Agent 代理框架。

- **后端**：Go 实现 Agent Engine、工具链与 Web 服务；ReAct 循环 + 上下文管理 + 历史压缩
- **前端**：浏览器富交互 GUI（Chat / Diff 预览 / HITL 审批），REST + WebSocket 双向通信
- **插件引擎**：四种驱动 —— Native Go 内置、MCP/Stdio（推荐）、HTTP Webhook、WASM(wazero) 沙盒
- **LLM 适配**：Anthropic / OpenAI / 本地模型（Ollama、vLLM）统一接口，流式输出 + Tool Call
- **安全**：Allow / Deny / Ask 三级策略 + Human-in-the-Loop 审批 + 审计日志
- **跨平台**：Linux(x86_64/arm64)、Termux(Android arm64)、Windows(x86_64)；CGO_ENABLED=0 单二进制交付，Web 资源 go:embed 内嵌，静态内存 ≤30MB

## 二、现状与环境

| 项 | 状态 |
|---|---|
| 工作目录 `CodeForge-go` | 空，全新项目 |
| Windows x86_64 | ✔ |
| Node v26.7.0 | ✔（本方案不依赖 npm 构建） |
| Git 2.55 | ✔ |
| Go 工具链 | ✘ 未安装，计划通过 winget 安装（失败则下载官方 zip 解压到 D:\APPS\go） |

## 三、关键落地决策（与原文档的差异调整）

| # | 决策 | 理由 |
|---|---|---|
| D1 | 前端零构建：纯 HTML/CSS/JS + go:embed，不引入 npm/xterm.js/Monaco 构建链 | 单文件交付、零依赖理念；xterm/Monaco 为后续增强 |
| D2 | 文件编辑采用「读→旧串替换→写」模式（同 Claude Code Edit），自动生成 Unified Diff 预览推送前端；不解析外部 patch 文件 | 比增量补丁应用更稳健，Diff 仅用于展示 |
| D3 | 第一阶段终端为异步命令执行+流式结果回传（exec.Command），交互式 PTY/ConPTY 列为后续增强 | 非交互执行已覆盖 95% Agent 场景，且纯 Go 无 CGO |
| D4 | LLM 适配不引入 SDK，原生 net/http + SSE 解析 | 控制依赖与内存占用 |
| D5 | custom.go = OpenAI 兼容端点适配（Ollama /v1、vLLM 均兼容） | 本地模型普遍提供 OpenAI 兼容接口 |
| D6 | 依赖最小化：gorilla/websocket、gopkg.in/yaml.v3、tetratelabs/wazero（全部纯 Go） | 满足 CGO_ENABLED=0 |
| D7 | 认证：启动时生成随机 Token，通过 URL `?token=` 注入前端，API 走 Bearer/查询参数校验 | 本地服务安全闭环 |
| D8 | MCP 驱动：单飞行串行 JSON-RPC（按行分隔 stdio），initialize → tools/list → tools/call | 兼容官方 MCP 规范 |
| D9 | WASM ABI：导出 `alloc(i32)→i32`、`manifest()→i64`、`call(ptr,len)→i64`（高32位=长度，低32位=偏移） | wazero 纯 Go 沙盒约定 |
| D10 | 检索工具为纯 Go 目录遍历+逐行扫描（跳过 .git/node_modules/dist/vendor） | 不依赖 ripgrep 二进制 |

## 四、实施阶段与文件清单

### Phase 0 环境准备
- winget 安装 Go（GoLang.Go），失败则手动下载 zip

### Phase 1 项目骨架与核心包
- `go.mod`（module codeforge）
- `config/config.go`：YAML 配置加载/合并/保存（default.yaml + local.yaml 覆盖）；API Key 环境变量回退
- `config/default.yaml`：server/llm/agent/security/plugins 默认配置
- `config/plugins.yaml`：插件示例（git_assistant MCP、github_tools MCP、sql_runner HTTP，默认 enabled=false）
- `pkg/platform/`：sys_common.go（接口）+ sys_windows.go（//go:build windows，ConPTY 预留、start 打开浏览器）+ sys_linux.go（!windows，bash/xdg-open，Termux $PREFIX 检测）+ launcher.go

### Phase 2 安全层与工具核心
- `pkg/security/policy.go`：Rule(Tools/Actions/Decision) 有序匹配 + 危险命令内置黑名单（rm -rf /、mkfs、format 等）强制 Deny
- `pkg/security/audit.go`：JSONL 审计日志（时间/工具/决策/审批/结果）
- `pkg/tools/tool.go`：Tool 接口与 ToolResult（完全按文档定义）
- `pkg/tools/registry.go`：注册中心 + Schema 生成（llm.ToolDef）
- `pkg/tools/executor.go`：超时控制、panic 恢复、输出截断、审计落盘

### Phase 3 内置工具（pkg/tools/builtin/）
- `file_ops.go`：read_file / list_dir / write_file / edit_file / delete_file（写前备份入撤销栈，导出 PreviewDiff 供 HITL 展示）
- `diff.go`：基于 LCS 的 Unified Diff 生成
- `terminal.go`：run_command（平台 shell 异步执行，流式收集，输出限幅）
- `search.go`：search_files 关键字检索（路径/glob/忽略目录）

### Phase 4 LLM 适配层（pkg/llm/）
- `provider.go`：Message/ContentBlock(text|tool_use|tool_result)/ToolDef/StreamEvent 统一模型 + 工厂
- `openai.go`：/chat/completions SSE 流式，tool_calls 分片拼装，历史格式互转（含 Ollama/vLLM）
- `anthropic.go`：/v1/messages SSE（content_block_delta / input_json_delta），messages-system 格式互转
- `custom.go`：OpenAI 兼容端点别名

### Phase 5 Agent 引擎（pkg/agent/）
- `agent.go`：ReAct 主循环（max_steps 限制）；每次工具调用走 policy：Allow→执行、Deny→拒绝结果、Ask→经 Approver 接口挂起等待 WS 审批
- `prompt.go`：中文 System Prompt 动态拼装
- `context.go`：Token 估算（字符/3）+ 超预算自动压缩（tool_result 内容省略/丢弃最旧轮次）
- `history.go`：会话 JSON 持久化（.codeforge/sessions/）+ 文件快照撤销栈 + undo 接口

### Phase 6 插件引擎（pkg/tools/plugins/）
- `loader.go`：plugins.yaml 解析（name/type/enabled/command/args/env/endpoint/security）
- `manager.go`：生命周期（Load/Stop/Reload），插件工具注册（含 requires_approval → Ask）
- `driver_mcp.go`：stdio JSON-RPC 2.0，initialize 握手 → tools/list → tools/call
- `driver_http.go`：POST {name,arguments} → {success,data,error}
- `driver_wasm.go`：wazero 沙盒（D9 ABI）

### Phase 7 Web 服务与入口
- `pkg/server/server.go`：路由 /api/* + /ws + 静态资源；Token 随机生成
- `pkg/server/middleware.go`：Token 校验 + 日志
- `pkg/server/ws_handler.go`：用户消息→Agent Run；事件流（text/tool_call/tool_result/hitl_request/done/error）实时推送；hitl_decision/cancel/new_session 处理；Approver 实现
- `pkg/server/api_handlers.go`：health/config(GET/POST, key 掩码)/tree/sessions/undo
- `cmd/agent/main.go`：flag 解析、装配依赖、启动、自动拉浏览器、信号优雅退出

### Phase 8 前端（web/dist + web/embed.go）
- `index.html` + `app.css` + `app.js`：深色主题；侧栏（会话列表/文件树懒加载）；聊天流式渲染；工具调用卡片（可折叠）；HITL 审批弹窗（含 Unified Diff 高亮）；设置弹窗（provider/base_url/api_key/model）

### Phase 9 构建与验证
- `go mod tidy && go vet && go build ./...`
- 单测：diff 生成、policy 三级判定
- 运行冒烟：/api/health、静态页、无 token 访问 /ws 被拒
- `Makefile`：windows/linux-amd64/linux-arm64/android-arm64 交叉编译（CGO_ENABLED=0，-s -w）

## 五、验证清单

1. `go build ./...` 全量编译通过（零 CGO）
2. 启动服务后：健康检查 200、浏览器自动打开 `http://127.0.0.1:8420/?token=...`、静态页可访问
3. 无 Token 访问 /api/config 与 /ws 被拒（401/升级失败）
4. 配置 LLM API Key 后：发送消息 → 流式回复 → 工具调用 → HITL 弹窗 → 批准执行 → 文件真实修改 + 审计日志落盘
5. plugins.yaml 开启 MCP 插件后 tools/list 正确注册
6. Makefile 三平台产物生成

## 六、风险与对策

- **winget 不可用** → 直接下载 go zip 解压至 D:\APPS\go，会话内 PATH 指定
- **无 LLM Key 无法端到端联调** → 冒烟验证服务/WS/静态；LLM 调用留待用户配置
- **WASM 无现成 .wasm 样例** → 实现驱动+ABI 文档化，运行时若无导出函数返回明确错误
- **网络拉取依赖失败** → 启用 GOPROXY=https://goproxy.cn
