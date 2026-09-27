# CodeForge-Go

> 跨平台 Web-Agent 代理框架与可扩展插件引擎 —— 对标 Codex / Claude Code 的轻量级、零依赖、单二进制实现。

用 Go 编写，浏览器即界面；`CGO_ENABLED=0` 编译出纯静态单文件，可直接跑在 Linux（x86_64 / arm64）、Android Termux（arm64）、Windows（x86_64）上。

---

## 核心特性

| 能力 | 说明 |
|---|---|
| **Agent 引擎** | ReAct（思考—行动—观察）主循环；上下文 Token 估算与自动压缩；会话 JSON 持久化 |
| **内置工具** | `read_file` / `list_dir` / `write_file` / `edit_file` / `delete_file` / `run_command` / `search_files` |
| **插件引擎** | 四种驱动：Native Go、**MCP/Stdio（推荐）**、HTTP Webhook、WASM（wazero 沙盒） |
| **多 LLM 适配** | Anthropic、OpenAI、本地模型（Ollama / vLLM 等 OpenAI 兼容端点），统一流式 + Tool Call；上游 429/5xx 自动指数退避重试 |
| **安全与 HITL** | Allow / Deny / Ask 三级策略 + 危险命令黑名单 + 人工审批弹窗 + JSONL 审计日志 |
| **Web GUI** | 会话列表、文件树、聊天流式渲染、工具调用卡片、Unified Diff 高亮审批、设置面板 |
| **多智能体协作** | 内置插件：主智能体可并行委派最多 5 个互不重叠的子智能体（探索 / 实现），三层防越权 |
| **单二进制交付** | Web 资源 `go:embed` 内嵌，无外部运行时依赖 |

---

## 快速开始

### 1. 构建

```bash
# 本机平台（Windows 产物为 bin/codeforge.exe —— go build -o bin/codeforge 不会自动补 .exe，
# 请显式写全后缀，否则会与 Makefile / cf.cmd 用的文件名不一致）
go build -o bin/codeforge.exe ./cmd/agent     # Windows
go build -o bin/codeforge     ./cmd/agent     # Linux / macOS

# 或使用 Makefile（本机构建会自动带上正确的后缀，见 `make build`）
make build
# 交叉编译（默认三件；Linux arm64 不在默认范围内，需要时 make linux-arm64）
make windows linux-amd64 android-arm64
```

### 2. 配置 API Key

程序启动时会**自动加载当前目录的 `.env`**（不覆盖真实环境变量），并自动生成 `config/local.yaml` 模板。

> **模型统一在「设置 → 模型」里管理**（服务端模型库 `config/models.yaml`，首次启动会把
> 当前生效模型自动种入列表）。对话框旁的模型名只是「当前生效项」的展示，列表的增删改查
> 一律走设置页；密钥明文只存在服务端（`models.yaml` / `providers.yaml` 与用户级
> `state.yaml`，均 0600 且都不入库），
> 前端只拿到 `key_set` 布尔值。也可任选其一手工配置：

```bash
# 方式 A：.env（推荐，模板已随仓库提供）
cp .env.example .env        # 填写 CODEFORGE_API_KEY / LLM_API_KEY

# 方式 B：真实环境变量
export CODEFORGE_API_KEY=sk-xxx        # 或 ANTHROPIC_API_KEY / OPENAI_API_KEY

# 方式 C：config/local.yaml（取消注释 llm 段）
```

切换模型服务商（如 OpenAI 兼容端点）**推荐直接在「设置 → 模型」里添加并应用**；
手工方式是在 `config/local.yaml` 中配置：

```yaml
llm:
  provider: openai
  base_url: "https://api.tokenrouter.com/v1"
  model: "z-ai/glm-5.3-free"
```

也可以启动后在界面「设置」中填写 —— 保存时写入**用户级运行状态** `~/.codeforge/state.yaml`
（程序不再改写 `config/local.yaml`，那是你的手写覆盖文件），前端只显示掩码。

### 3. 启动

```bash
# 方式一：用启动器（推荐 —— 与当前目录无关，内部会自动切到项目根目录）
cf                 # 启动（自动打开浏览器）
cf -no-open        # 不打开浏览器
cf stop            # 停止
cf restart         # 停止后重启

# 方式二：直接运行（注意：CWD 必须是项目根目录，即 config/ 与 .env 所在的那一层）
./bin/codeforge -config config

# Windows PowerShell 等价写法：
.\bin\codeforge.exe -config config

# 方式三：Makefile（构建 + 启动）
make run
```

> **关于 `cf`**：`cf.cmd` 是仓库根目录下的启动器，它先 `cd` 到自身所在目录再执行 `bin/codeforge.exe`，
> 因此**从任何目录调用都不会踩 CWD 的坑**（原因见下方警告块）。用法就是 `cf` / `cf stop` / `cf restart`。
>
> - 想在任何目录直接用 `cf` 命令：把项目根目录加入 PATH，或在 PowerShell 配置文件中加一行
>   `function cf { & "<项目根目录>\cf.cmd" @args }`。
> - 用 Git Bash 的话，等价写法是 `alias cf='/c/.../CodeForge-go/cf.cmd'`，或直接调用完整路径。

启动日志会打印生效的配置来源与访问地址：

```
已加载环境变量文件：.env
本地覆盖配置：config\local.yaml
CodeForge 已启动：http://127.0.0.1:8420/
平台：windows｜工作目录：...｜可用工具：7 个
模型：z-ai/glm-5.3-free（provider=openai，API Key 已加载）
```

浏览器会自动打开 `http://127.0.0.1:8420/`（加 `-no-open` 可禁用）。

> **工作区选择会记住**：在界面选择工作区后，绝对路径自动写进 `~/.codeforge/state.yaml` 的
> `agent.work_dir`，重启后自动恢复，不必每次重新选。目录被删时启动会打告警并按
> 「未选择工作区」处理；也可以启动时用 `-workdir <目录>` 显式指定。

> **⚠️ 必须在项目根目录下启动**（`CodeForge-go/`）。
> `-config` 与 `.env` 的查找路径都是**相对当前工作目录（CWD）**解析的，与可执行文件放在哪里无关。
> 如果在 `bin\` 目录里直接跑 `.\codeforge.exe`，它会去找 `bin\config\`（没有 `default.yaml`），
> 于是 provider / model 全部落回内置默认值（`anthropic` + `claude-sonnet-4-20250514`），
> 项目根目录的 `.env` 也加载不到 —— 程序仍能正常启动，但配置是错的。
> 程序会在这种情况下打印 `警告：配置目录 ... 下没有 default.yaml`，
> 也可以在启动日志里核对 `模型：` 那一行是否符合预期。

> **端口只由全局配置决定**：`config/default.yaml` 的 `server.port`（可被 `config/local.yaml` 覆盖）。
> 程序**不会自动换端口** —— 端口被占用时直接报错退出，请先执行 `./bin/codeforge stop`
> （或 `make stop`）关闭旧实例，或修改配置中的端口。

### 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-config` | `config` | 配置目录（**相对 CWD** 解析） |
| `-workdir` | 当前目录 | Agent 工作目录 |
| `-no-open` | `false` | 不自动打开浏览器 |

子命令：`codeforge [start]`（默认）/ `codeforge stop` / `codeforge restart`。

> 监听端口没有命令行参数 —— 它是全局配置项，只在 `config/default.yaml`（`server.port`）中声明。

---

## 各平台启动

产物与平台的对应关系（全部为 `CGO_ENABLED=0` 纯静态单文件，自带内嵌前端，**不需要**额外拷贝 `web/`）：

| 平台 | 产物 | 说明 |
|---|---|---|
| Windows x64 | `bin/codeforge.exe` | 本机构建产物；`make windows` 另出 `codeforge-windows-amd64.exe` |
| Linux x86_64 | `bin/codeforge-linux-amd64` | 云主机 / WSL |
| Linux arm64 | `bin/codeforge-linux-arm64` | 树莓派 / ARM 云主机；**默认不编**，需要时 `make linux-arm64` |
| Android Termux arm64 | `bin/codeforge-android-arm64` | `GOOS=android` 专为 Termux 编译 |

### 三条通用规则

1. **`-config` 与 `.env` 都相对「当前工作目录」解析**，与二进制放在哪里无关。
   所以要么在放着 `config/` 的那一层执行，要么用绝对路径 `-config /opt/codeforge/config`。
   跑错目录时会打印 `警告：配置目录 ... 下没有 default.yaml`。
2. **端口固定 8420**（`config/default.yaml` 的 `server.port`），不会自动换端口；被占用则直接报错退出。
3. **默认只监听 `127.0.0.1`**。要从其它机器访问，需把 `server.host` 改为 `0.0.0.0`。

### Windows x64

```powershell
cd C:\Users\26536\Desktop\Code\CodeForge-GO\CodeForge-go
cf                                    # 启动器，推荐；等价于 .\bin\codeforge.exe -config config
cf stop                               # 停止
cf restart                            # 停止后重启
```

浏览器会自动打开（`-no-open` 可禁用）。

### Linux x86_64 / arm64

```bash
chmod +x codeforge-linux-amd64        # arm64 换成 codeforge-linux-arm64（默认不编，先 `make linux-arm64`）
./codeforge-linux-amd64 -config config
```

- **无桌面环境的服务器**：加上 `-no-open`（否则只会在日志里提示找不到浏览器），然后从浏览器访问
  `http://<服务器IP>:8420/` —— 记得先把 `server.host` 改成 `0.0.0.0`。
- 自动开浏览器依次尝试 `xdg-open` → `open`（macOS），都没有时会提示手动访问。
- `codeforge stop` / `restart` 在各平台通用：优先向实例发带令牌的 `POST /api/shutdown` 优雅关闭，
  失败再按 `~/.codeforge/run.json` 里的 PID 强制结束（PID 失效时按端口定位，POSIX 上依次尝试 `lsof` / `ss`）。
  该文件只描述当前这一个进程，退出即删。

### Android Termux arm64

```bash
pkg install -y termux-tools           # 可选：提供 termux-open-url，用于自动拉起浏览器
chmod +x codeforge-android-arm64
./codeforge-android-arm64 -config config
```

- `GOOS=android` 的产物专为 Termux 编译；`codeforge-linux-arm64` 在 Termux 里同样可以运行。
- 在 Termux 中平台名会被识别为 `termux`（依据 `$PREFIX` 是否含 `com.termux`）。
- 想关掉自动开浏览器用 `-no-open`。

#### termux-tools 探测：为什么「明明装过」也说没装

`/api/termux/tools` 的「已安装」判定**显式把 `$PREFIX/bin` 并入查找路径**，
并探测三个代表命令（`termux-open-url` / `termux-storage-get` / `termux-setup-storage`）。

只 `LookPath` 进程 PATH 里的 `termux-open-url` 会在一种常见情况下误判：从
Termux 外部拉起 CodeForge（Termux:API、桌面快捷方式、某些启动器）时进程的
`PATH` 里没有 `$PREFIX/bin`，于是明明装过也报「未安装」，启动反复弹安装建议。

#### 换背景图：termux-storage-get 失败会自动降级

安卓系统选图走 `termux-storage-get`（SAF 选择器）。它失败时**不再**甩一句
「需要 termux-tools」（早先一律如此，真实原因全被掩盖），而是：

1. 如实区分「命令确实不在」与「命令在但这次调用失败」，并把上游的原始报错带出来；
2. **回落前端内置文件选择器**（起点优先 `~/storage/shared`，未授权则退回 `~`），
   界面上给出 info 说明为什么换了选图方式 —— 无论如何都能换图；
3. 选中的图落在**工作区下的绝对路径** `.codeforge/background_user`
   （`data_dir` 是相对值，直接拼出来会依赖进程 CWD，工作区切换后就落错地方）。


### 最小化部署

只需要这三样，放到目标机器的同一个目录下即可：

```
/opt/codeforge/
├── codeforge-linux-amd64     # 对应平台的二进制
├── config/                   # default.yaml + local.yaml（建议整目录拷贝）
└── .env                      # 只放 API Key 即可
```

启动后 API Key 也可以直接在界面「设置」里填，会写入用户级 `~/.codeforge/state.yaml`（前端只显示掩码）。

---

## 目录结构

```
CodeForge-go/
├── cf.cmd                # 启动器：与 CWD 无关地启动 / 停止 / 重启（见「启动」一节）
├── cmd/agent/            # 主程序入口
├── config/               # YAML 配置解析 + default.yaml + plugins.yaml
│                         #   （local.yaml 自动生成、被 gitignore 忽略）
├── tests/                # pytest LLM 端点测试（OpenAI 兼容协议）
├── pkg/
│   ├── agent/            # ReAct 主循环、上下文压缩、会话历史、System Prompt
│   ├── llm/              # LLM 统一适配层（provider / openai / anthropic / sse）
│   ├── security/         # 三级策略引擎 + 审计日志
│   ├── server/           # HTTP & WebSocket 服务、REST 接口、HITL 审批
│   ├── platform/         # 跨平台抽象（Linux / Termux / Windows）
│   └── tools/
│       ├── tool.go       # Tool 接口 + JSON Schema 构造器
│       ├── registry.go   # 工具注册中心
│       ├── executor.go   # 安全执行器（超时 / panic 恢复 / 限幅 / 审计）
│       ├── builtin/      # 内置工具 + LCS Unified Diff
│       └── plugins/      # 插件引擎（mcp / http / wasm 驱动）
└── web/                  # go:embed 前端资源（零构建链）
    ├── embed.go          # //go:embed dist
    ├── dist/             # index.html / app.css / ui.js / drag.js
    └── test/             # 前端 Markdown 渲染器回归测试（Node，见「测试」一节）
```

---

## 配置与密钥（自动生成）

遵循「**密钥不入库、忽略文件可重建**」原则。

### 分层与写入者

**每个文件只有一个写入者** —— 这是这套配置结构的硬规则。合并次序是
`default.yaml → local.yaml → ~/.codeforge/state.yaml`（后者覆盖前者），
所以界面上的改动永远赢过手写的静态覆盖，而手写的 `local.yaml` 不会被程序改写。

| 文件 | 位置 | 写入者 | 是否入库 |
|---|---|---|---|
| `default.yaml` | `config/` | 代码 / Git | ✅ |
| `plugins.yaml` | `config/` | 开发者（同时 `go:embed` 为内建默认） | ✅ |
| `local.yaml` | `config/` | **用户手工编辑** | ❌ |
| `models.yaml`、`providers.yaml` | `config/` | 设置页 | ❌（含密钥） |
| `.env` | 项目根 | 用户（含 pytest 在线用例用的 `LLM_*`） | ❌（含密钥） |
| `state.yaml` | `~/.codeforge/` | **程序**（运行状态：当前模型/工作区/外观/开关） | 不在仓库内 |
| `run.json` | `~/.codeforge/` | 程序（仅描述当前进程，退出即删） | 不在仓库内 |
| `data.db` | `~/.codeforge/` | 程序（会话、记忆，跨工作区共享） | 不在仓库内 |

> 模型/供应商目录与 `plugins.yaml` 不合并成一个文件，是为了守住密钥边界：
> `plugins.yaml` 必须进 Git 并被 `go:embed` 打进二进制（裸 exe 才自带 MCP 服务），
> 而 `models.yaml` / `providers.yaml` 含明文密钥必须被忽略 —— 同一份文件做不到两件事。

### 被忽略的内容（`.gitignore`）

| 模式 | 说明 |
|---|---|
| `config/local.yaml`、`config/*.local.yaml` | 本地配置覆盖，**由用户编辑**（程序只读，不再写入） |
| `config/models.yaml` | **模型库**（设置页统一管理），含各模型密钥明文，0600 |
| `.env`、`.env.*`（保留 `*.example`） | 环境变量文件 |
| `*apikey*`、`*api_key*`、`*api-key*` | 命名约定兜底 |
| `*secret*`、`*credential*`、`*credentials*` | 命名约定兜底 |
| `secrets/`、`.secrets/`、`.credentials/` | 密钥目录 |
| `.codeforge/`、`/bin/` | 运行时数据与构建产物 |

### 自动生成（克隆后无需手动补齐）

| 文件 | 生成方式 |
|---|---|
| `config/local.yaml` | **程序首次启动自动生成**带注释模板（`config.EnsureLocalTemplate`，权限 0600）；界面「应用模型」时写入生效配置 |
| `config/models.yaml` | 空库时自动把当前生效模型**种子**为第一条；之后由设置页增删改 |
| `.env` | `cp .env.example .env` 后填写；**服务启动与 pytest 均会自动读取**（真实环境变量优先级更高） |

### 模型的两条数据链路（单链路设计）

- **设置 → 模型 → 模型列表**：唯一管理入口，数据源是服务端 `config/models.yaml`
  （`/api/models/list|save|delete|apply`）。列表可添加多个模型，逐条「应用」切换生效配置。
- **对话框旁的模型按钮**：只读展示「当前生效」的模型（`/api/config`），不能从这里切换。
- 两条链路的**唯一交点是「应用」动作**：列表条目 → 覆写 `llm` 段 → 热切换 Provider → 写回 `local.yaml`。
- 密钥明文只在服务端流转；`/api/models/list` 返回的是脱敏视图（只有 `key_set` 布尔值）。
  脱敏视图同时带回 `ctx_in` / `ctx_out`（上下文与输出上限），供设置页表单回显与上下文进度条取「模型窗口」。
- **当前生效模型的密钥在设置页可直接查看**：`/api/models/list` 对「已应用/生效」的那一条
  额外附带 `key_plain` 明文（仅此一条），编辑该模型时密钥框默认掩码显示、点「眼睛」可切明文；
  其余模型仍只有 `key_set`，前端不接触其明文。
- **「测试连接」**（`/api/models/test`）：默认用表单里填的密钥探测上游；表单没给 key 时
  回退到模型库当前模型条目（与「应用」同源的密钥解析），保证测试与对话一致。
  探测用 `max_tokens=1` 的最小请求，遇 5xx/429 等上游瞬时故障会原样回报，并附带提示
  「实际对话使用的生效配置可能与此不同」。
- **批量添加：先拉取上游模型列表再勾选**（`/api/models/discover` + `/api/models/save_batch`）：
  「添加模型」面板里的「获取模型列表」会请求上游 `GET <base_url>/models`
  （`base_url` 已含 `/v1` 时优先直连，遇 404/405 再回退补一层 `/v1`），把返回的模型 id
  列出来供筛选 / 全选，多选后一次性入库。上游响应按「找数组 → 逐项取 id」宽容解析，
  兼容 OpenAI `data[].id`、Anthropic `data[].display_name`、Ollama `models[].name` 与裸数组。
  ⚠️ 两个容易踩的点：① **表单没填地址或密钥时会回退到「当前生效」的配置**并发起真实请求
  （响应里的 `key_from_active` 会如实告知用了哪一份，不是无副作用的探测）；
  ② **已存在的 id 一律跳过而不覆盖** —— 批量语义是「补缺」，
  覆盖会悄悄改掉手工调过的地址 / 密钥 / 上下文上限（响应回 `added` / `skipped` / `failed` 三类计数）。

所有被忽略的文件都可安全删除：重启程序会重建模板，或由界面「设置」重新生成。

> 校验命令：`git check-ignore -v config/local.yaml .env`

---

## 安全模型

1. **三级策略**：`ALLOW`（只读自动放行）/ `DENY`（危险命令黑名单强制拒绝）/ `ASK`（人工审批）。
2. **工作区边界**：文件与命令工具的目标路径（含 `run_command` 的 `cwd`）解析后必须落在
   `agent.work_dir` 之内。相对穿越（`../../x`）与绝对路径越界都会被直接拒绝，
   经由工作区内软链接指向区外的路径同样拦截（含目标文件尚不存在的新建写入）。
   **只读工具是自动放行的**，所以这道边界才是防止「工作区外文件被静默读入上下文」的关键。
   确实需要操作工作区外的文件时，在 `config/local.yaml` 里设置
   `security.allow_outside_workspace: true`（默认 `false`）。
3. **越界访问强制人工审批**：执行器在任何权限模式（只读 / 请求 / 自主）下都会拦截
   「策略放行但目标越出工作区」的调用并升级为 ASK——包括 `run_command` 的命令文本
   引用了区外路径（绝对路径、`../` 穿越、`~`、重定向目标、`cd` 逃逸、`$VAR`/`%VAR%`
   动态路径等启发式扫描）。用户在审批弹窗中批准后，仅该一次调用放行执行；
   Deny（黑名单）维持原判，审批不能解锁。
4. **危险命令黑名单**：`rm -rf /`、`mkfs`、`format`、`dd of=/dev/`、fork bomb 等强制 Deny。
5. **Human-in-the-Loop**：Ask 动作挂起 Agent，前端弹窗展示命令详情与 Unified Diff，批准后才执行。
6. **审计留痕**：每次工具调用写入 `.codeforge/audit.jsonl`（时间 / 工具 / 决策 / 审批 / 结果 / 耗时）。
7. **本地鉴权**：启动生成随机 Token，通过 **HttpOnly Cookie** 下发；Token 不出现在 URL 或界面中。
8. **可撤销**：文件写入前记录快照，界面「撤销」可回滚最近一次修改。

---

## 插件配置

编辑 `config/plugins.yaml`（默认全部 `enabled: false`）：

```yaml
plugins:
  - name: "git_assistant"
    type: "mcp"                 # mcp | http | wasm | native
    enabled: true
    command: "python"
    args: ["./plugins/git_mcp_server.py"]
    security_policy:
      requires_approval: true   # 该插件所有工具强制人工审批
```

MCP 驱动实现 stdio JSON-RPC 2.0 握手：`initialize → tools/list → tools/call`。

---

## 内置插件

内置插件是**进程内实现**、可开关的增强能力（区别于上面 `plugins.yaml` 里的外部插件）。
开关在 `config/default.yaml` 的 `builtin_plugins` 段，也可在「设置 → 插件」里热切换
（改的是 `config/local.yaml`，无需重启）：

| 插件 | 默认 | 作用 |
|---|---|---|
| `skill_creator` | 开 | 让 AI 在对话中创建 / 迭代工作区技能（`.codeforge/skills/<名>/SKILL.md`） |
| `multi_agent` | 开 | 允许主智能体通过 `delegate_subagents` 工具并行委派子智能体 |

### 子智能体（多智能体协作）

设置页「设置 → 子智能体」可热调，配置落在 `subagents` 段：

```yaml
builtin_plugins:
  multi_agent: true       # 总开关：关闭后 delegate_subagents 工具不再注册，模型无从调用
subagents:
  max_concurrent: 5       # 一次委派最多并行几个子智能体（硬上限 5，只能调小）
  max_steps: 0            # 单个子智能体的 ReAct 步数上限；0 = 跟随 agent.max_steps（硬上限 60）
  allow_write: true       # 允许 implement 子智能体写入 / 编辑文件；false 则子智能体一律只读
  allow_delete: true      # 允许子智能体删除文件（allow_write=false 时自动无效）
  allow_memory: true      # 允许子智能体写入用户记忆（save_memory）
```

总开关与「设置 → 插件」里的 `multi_agent` 是**同一份配置**，两个入口不会互相矛盾
（设置页把被改动的那一项单独提交，不会用界面上的旧值覆盖别处刚改的开关）。
并发上限的「滑条上限 / 工具参数校验 / 调度器校验」三处同源于 `config.SubagentConcurrencyCap`，
不会出现「界面写 5、实际只跑 2」的错位；越界值按硬上限收敛而不是拒绝保存。

> `max_steps` 默认 **0 = 跟随主循环**的步数上限。早先子智能体被写死 8 步，
> 探索类任务（读几个文件、搜几处、交叉验证）经常在半途被硬停，模型被迫交一份
> 「还没看完」的结论，主智能体再接着做等于把活儿又干一遍。压成本时在设置页
> 「设置 → 子智能体 → 单任务步数上限」单独调小，而不是靠一个藏在代码里的魔数。

**三层防越权**（都不依赖模型自觉）：

1. **调度前校验**：一次最多 5 个、子任务 id 唯一且必填、`implement` 必须声明 `paths`、
   不同子任务的 `paths` 不得重叠（按 `/` 边界与小写归一比较，`partA` 与 `partA2` 视为不同目录）。
2. **工具白名单**：`explore` 只有读文件 / 列目录 / 搜索（+ 写记忆）；`implement` 在此基础上
   按 `allow_write` / `allow_delete` 增补写 / 编辑 / 删除。**任何模式都不会暴露**
   `run_command`、`delegate_subagents`（防递归委派）、`create_skill` 或插件工具。
   策略只做减法 —— 所有开关都只能缩小能力，不能扩大。
3. **执行层路径围栏**：子智能体的文件工具被限制在它声明的 `paths` 之内。
   即使人工审批放行了「工作区越界」，也**不会**穿透子任务之间的围栏。

子智能体与主循环的其余一致性（都曾因缺失而让子任务白跑）：

| 继承项 | 说明 |
|---|---|
| 上下文窗口 | 子智能体与主循环用**同一个**模型窗口算压缩线。`contextWindow` 是 Agent 上的 atomic，`New()` 不填 —— 不显式继承的话子智能体拿到的窗口是 0，压缩线会回退到写死的 `context_token_budget`（120000）；模型真实窗口若更小（32k / 64k 端点），子智能体会堆到上游拒绝才压缩，子任务直接失败。 |
| 工具可见面 | 用户在「设置 → 常规」隐藏过的工具（`agent.hidden_tools`）不会又出现在子智能体的工具定义里。 |
| 内置插件开关 | **刻意不继承**：白名单里没有 `create_skill` / `delegate_subagents`，继承了会让 System Prompt 注入「你可以创建技能 / 委派子智能体」，模型反复调用不存在的工具直到步数耗尽。 |


被限制为只读时，子智能体的提示词会同步改成「给出需要主智能体落地的改动清单」，
确保**措辞与实际授予的工具集一致** —— 否则模型会反复调用不存在的写工具而空转。

子会话不写入会话历史（`persist=false`），只有摘要回传给主智能体；父会话里看到的是
「委派了一次、收获 N 条摘要」，而非子智能体的完整过程。

---

## 打断之后怎么接着跑

一轮被**打断**（Esc）、**上游报错**、或**刷新页面**之后，界面会挂出一个「继续」圆环。
它的语义是「**从断点接着往下做**」，不是「重来」：

- **不截断历史**：已经跑完的工具调用与结果全部留在上下文里；
- **不回退文件**：已经改过的文件保持原样；
- **不新增提问**：⚠️ 这里踩过坑。旧实现是让前端发一句字面量「继续」当普通用户消息，
  于是历史里留下一条**用户从未说过**的假提问 —— 历史回放时它仍在（用户会问
  「我什么时候说过继续」），而且模型分不清「被打断后接着跑」与「用户新提了一个要求」，
  容易把已经做完的部分再做一遍。现在服务端 `ContinueTurn` 不追加任何消息，
  只把「本轮是继续上一轮」作为**一次性系统提示**挂上（跑完即清、不落库、不进 `Messages`），
  并在续跑前补齐末尾悬空的 `tool_result`（上游对消息序列有硬约束，带着残缺序列请求会被 400 拒绝）。

要**破坏性地重来**，走那两个显式入口：「重新生成」（丢弃最后一轮回复）或
「编辑重发」（改写某条历史提问并截断其后一切，同时按检查点回退文件改动）。

判据在服务端算（`Agent.UnfinishedTurnAnchor`，随 `checkpoints` 事件的 `resume_back` 下发），
页面刷新后视图无状态，前端自己猜不出来。

---

## 系统通知

任务完成或执行失败时发一条系统通知（Windows 走 Toast），方便切出去做别的事时知道进度。

**「设置 → 常规 → 系统通知」是总开关**：关闭后不再发送任何通知。开关落在 `local.yaml`：

```yaml
notify:
  enabled: true           # 总开关（缺省开启）
  min_interval_ms: 3000   # 节流窗口：这段时间内只提醒一次，避免连续任务刷屏
```

即使开着，也只有**窗口切到后台时**才提醒 —— 前端在页面可见性变化与 WebSocket 建连时上报
`{type:'visibility', hidden}`，窗口可见时一律不打扰（未知状态按「可通知」处理，兼容非浏览器客户端）。
Windows 上通知固定使用同一个 `Tag` / `Group`，因此新通知会**替换**旧的而不是在操作中心越积越多
（曾出现过「无操作也弹通知」的堆积问题，根因就是空 Tag/Group 无法去重），并附加 5 分钟过期时间。

排查「为什么没收到通知」时看服务端日志：判定收敛在一个函数里，跳过时会打印
`[notify] 跳过系统通知：<具体原因>`（前台可见 / 开关关闭 / 节流窗口内），可自证。

---

## 前端渲染注意事项

前端是**零构建链**的原生 HTML / CSS / JS（`web/dist/`，由 `go:embed` 内嵌），
Markdown 由 `ui.js` 里的手写 `renderMD()` 负责，工具卡片文案由 `toolLabel()` 统一生成。
改这块时有几个反直觉的坑：

### 1. `.msg-assistant.md` 必须重置 `white-space`

`.msg-assistant`（聊天气泡）为了让纯文本消息保留换行，设置了 `white-space: pre-wrap`。
而 `renderMD()` 生成的 HTML 是用 `'\n'` 把各个块级元素拼起来的 ——
在 `pre-wrap` 下这些**只用于排版的分隔换行会变成真实换行**，
每个块之间凭空多出约 24.5px 的空隙（典型症状就是「大标题和正文之间的间距特别大」）。

所以 `.msg-assistant.md` 必须显式重置，把间距交还给 CSS 的 `margin`：

```css
.msg-assistant.md { white-space: normal; }
```

### 2. 块级语法必须在行内规则**之前**抽取

`renderMD()` 是一条纯正则流水线，**顺序即语义**：

1. **先抽取** fenced 代码块与数学公式，替换成 `\u0000B<n>\u0000` 占位符；
2. 再跑行内规则（`**粗体**` / `*斜体*` / `` `代码` `` / 链接）；
3. 最后统一还原占位符。

若不先抽取，公式里的 `*`、`` ` ``、`_`、`\` 会被当成 Markdown 语法处理 ——
例如 `$a*b*c$` 会被拆成 `<em>`、`$x_1$` 的下标会被吞掉。

另外，占位符承载的块级元素一律用 `<span>` + CSS `display:block`，**不要用 `<div>`**：
段落包裹那一步会把裸文本包进 `<p>`，而 `<div>` 落进 `<p>` 属于非法嵌套，
浏览器会「替你修正」DOM，排版随即错乱。

### 3. 工具卡片文案集中在 `toolLabel()`

聊天流里每次工具调用的可见文案由 `ui.js` 的 `toolLabel(name, input)` 统一生成，
把「调用了 read_file」这类机器口径换成中文短语 + 目标：

| 工具 | 卡片文案 |
|---|---|
| `read_file` | 读取了文件 `main.go` |
| `list_dir` | 查看工作区 |
| `search_files` | 搜索工作区 |
| `write_file` | 创建 `a.txt` |
| `edit_file` | 编辑 `z.py` |
| `delete_file` | 删除了文件 `old.log` |
| `run_command` | 运行 `go build ./...` |

约定：**路径只取最后一段**（长路径会把卡片撑宽，完整参数仍保留在 `title` 悬浮提示里）；
文件名超过 60 字符、命令超过 80 字符会被截断并补 `…`；`list_dir` / `search_files` 面向整个工作区，
不带具体目标；参数缺失时降级为纯短语（如「创建了文件」）；
**新增工具时记得在 `switch` 里补一个 `case`**，否则会回退成「调用了 &lt;name&gt;」。

### 4. 底部栏的上下文占用进度条

左栏底部「设置」按钮右侧有一条进度条（`#ctx-meter`），宽度 = **送模占用 / 压缩线**，
点击向上弹出明细（模型、送模占用、占比、模型窗口、输出预留、历史原文、历史消息、已摘要）。

- **口径与「自动压缩」完全一致**：数据由 `agent.ContextStat(sess)` 统一产出，`Used/Budget`
  就是压缩判定的输入。因此条子满 100% 的精确含义就是「下一次请求会触发摘要压缩」，
  不是另算的一套数字。（历史上二者各算各的，进度条会骗人。）
- **压缩线不是固定 120000**：优先按模型窗口算
  `(窗口 − 输出预留) × context_compress_ratio`（缺省 0.95）；窗口未知才回退到
  `agent.context_token_budget`。输出预留见下条。
- **`Used` 与 `Raw` 要分清**：`used` 是**实际送模**的估算（已被摘要替换掉的部分不计入），
  `raw` 是**完整历史**的估算。压缩发生后 `raw > used`，两者都会显示出来。
- **两个状态位别再混用**（这里踩过一次）：
  | 字段 | 含义 |
  |---|---|
  | `over_budget` | `used` 越过压缩线（本步送模会触发压缩） |
  | `compressed` | 会话里**已经存在**摘要（压缩发生过） |
  刚超线的那一瞬间是 `over_budget=true` 而 `compressed=false`，二者不是同义词。
- **超线不越界**：`percent` 照实返回（可 >100），前端只把**条宽**夹到 100%，数字照实显示。
- **输出预留（`reserve`）** 解释「压缩线为什么小于模型窗口」：一次请求的总量是「输入 + 输出」，
  而压缩线只管输入。预留 = `min(输出上限, 窗口/2)`，避免「输入按窗口 95% 算、加上输出必然超窗」
  —— 那样压缩反而变成超窗的帮凶。
- 数据经 WebSocket `{"type":"context"}` 下发（服务端 `Server.contextUsage`），
  时机为新建会话 / 切换会话 / 每轮 `idle` 之后；点开明细时前端还会主动拉一次。
- **颜色阈值** 70% 警告、90% 危险，与 `fmtTokens()` 的缩写（`12.3k` / `1.2M`）一同在
  `web/test/render_md.test.js` 里锁了契约（含 `over_budget`/`compressed` 的区分）。

> Token 估算（`agent.EstimateTokens`）口径刻意保守：**CJK 按 1 字 ≈ 1 token，其余按 4 字 ≈ 1 token**。
> 宁可高估——低估会导致「估算还没到阈值、请求已被上游拒」，而压缩是幂等的，提前压只多花点摘要成本。
> （旧实现统一「字符数 ÷ 3」，对中文低估约 3 倍，长中文会话会稳定超窗。）

### 5. 编辑重发：破坏视图的动作要最小且可回退

点用户气泡旁的「编辑」改写并重发时，界面要立刻给出反馈，于是有一段「乐观改动」。
这里的原则是：**只删被编辑那条之后的节点，那一条本身就地改写**。

早先的实现是「把被编辑那条连同其后全部删掉，再补一条新气泡」。只要中途 `wsSend` 失败、
或服务端在 emit `edit` 之前就 `return error`（`back` 定位不到那条消息），
那条消息就永远回不来 —— 用户看到的正是「被编辑的消息直接就没了」。

配套的两条：

- **`pendingEdit` 标志**：编辑重发已发出但还没收到服务端权威快照时置位；
  收到 `history` / `idle` 清除；收到 `error` 且仍置位 → 主动 `load_session` 拉一次快照，
  把视图拉回库里的实际样子（否则界面会停在乐观改动后的状态，与真实历史对不上）。
- **倒计数排除 steer**：`.msg-steer` 视觉上也是一条用户消息（带 `.msg-user` 类），
  但插话不是「提问」。服务端算 `back` 用的是 `isUserQuestion`（排除 `OriginSteer`），
  前端把 steer 算进去就会数错条数、截错位置。

### 6. 改完必须重新编译

前端属于 `go:embed` 资源，`ui.js` / `app.css` 的改动**不会热更新**，必须重新 `go build` 才会生效。

```bash
node web/test/render_md.test.js      # 或 make test-web —— 先跑回归测试
make build && cf restart             # 再重编译并重启
```

### 7. 目前未支持的语法

图片、嵌套列表、任务列表（`- [ ]`）尚未实现，遇到时降级为普通文本 / 单层列表。

---

## 测试

### Go 单元 / 集成测试

```bash
go vet ./...
go test ./...
```

覆盖：Unified Diff 生成、安全策略三级判定、危险命令拦截、Cookie 鉴权、WebSocket 端到端 Agent 事件流、
子智能体策略（并发上限收敛 / 步数继承主循环 / 上下文窗口与可见面继承 / 提示词与实授工具集一致 / 路径围栏）、
子智能体设置接口（部分更新语义与落盘）、模型发现与批量添加（上游响应宽容解析、
已存在跳过不覆盖、响应不泄露明文密钥）、系统通知的四道闸门、
「继续」不追加假提问（agent 层 + WS 协议层各一条）、termux-tools 探测与选图降级路径。

### 前端渲染器测试（Node，零依赖）

`web/dist/ui.js` 里的 `renderMD()` 是手写的零依赖 Markdown 渲染器，改它很容易悄悄弄坏别的语法，
所以单独配了一层回归测试 —— **纯 Node 运行，不需要浏览器、不消耗任何额度**：

```bash
node web/test/render_md.test.js       # 或 make test-web
```

它不加载整个 `ui.js`（那个文件加载时会访问 DOM），而是从源码里按花括号配对截出
`escapeHtml` / `renderMD` / `toolLabel` / `countLines` / `fmtTokens` / `wsDisplayName`
六个纯函数注入调用，覆盖：

- 基础语法：标题 / 粗斜体 / 行内代码 / 链接 / 引用 / 分隔线 / 列表 / 段内换行 / HTML 转义防注入
- 表格：三种对齐、列数与行数、不被包进 `<p>`、与相邻标题各自成块、缺少分隔行时不误判
- 公式：`$…$` `$$…$$` `\(…\)` `\[…\]` 四种写法，以及 `$a*b*c$` 不被吃成斜体、
  `$x_1$` 下标不被吞、**「价格 $5 到 $10」不被误判成公式**、代码块内公式不处理
- 工具卡片文案 `toolLabel()`：文件名只取末段、`list_dir` / `search_files` 不带目标、
  缺参数时降级为纯短语、未知工具回退为「调用了 &lt;name&gt;」、超长命令与文件名截断
- 编辑重发契约：目标气泡**就地改写**（不删了再加）、steer 不参与倒计数、
  `pendingEdit` 在失败时回补权威快照
- 「继续」按钮：发 `continue_turn` 而不是字面量「继续」；服务端 `ContinueTurn`
  不追加用户消息，「本轮是继续上一轮」只作为一次性系统提示
- 子智能体：步数继承主循环（不再写死 8）、继承上下文窗口与工具可见面、
  不继承内置插件开关、策略只取一次快照
- Termux：`$PREFIX/bin` 并入命令查找、缺哪个命令逐项报、`termux-storage-get`
  失败回落内置选择器、落盘路径是工作区下的绝对路径
- `app.css` 样式契约：`.msg-assistant.md` 必须重置 `white-space`（见「前端渲染注意事项」一节）
- 设置面板契约：通知总开关的文案与标记、子智能体页四项控件齐全、改动按**单项提交**、
  失败回滚、总开关/禁写时联动置灰；模型「获取模型列表」面板的存在性、已在库条目标 `added`
  且不可勾选、换模型/保存时收起面板防勾选串味

### LLM 端点测试（pytest）

验证任意 OpenAI 兼容端点（默认 `https://api.tokenrouter.com/v1` + `z-ai/glm-5.3-free`）是否满足 CodeForge 的协议要求。

```bash
cp .env.example .env          # 填写 LLM_API_KEY（.env 已被 .gitignore 忽略）
pytest tests/ -v -s
```

| 用例 | 校验点 |
|---|---|
| `test_models_endpoint` | `GET /models` 返回可用模型列表 |
| `test_chat_completion_non_stream` | 非流式对话返回非空文本与 `usage` |
| `test_chat_completion_stream` | SSE 增量拼装正确，且以 `[DONE]` 结束 |
| `test_tool_calling` | 函数调用返回合法 `tool_calls` 与 JSON 参数 |
| `test_codeforge_payload_shape` | 复现适配层载荷（tools + stream），含多轮工具结果回填 |
| `test_invalid_api_key_rejected` | 错误密钥返回 401 / 403 |

上游网关偶发 `429 / 502 / 503` 时用例会自动指数退避重试，重试耗尽才判定失败。

### 真实 LLM 端到端测试（Go）

用真实端点跑通完整链路：用户消息 → ReAct 循环 → 工具调用（只读自动放行）→ 工具结果回填 → 写操作触发 HITL 审批 → 批准 → 文件真实变更 → 审计落盘。

```bash
make test-e2e
# 等价于：
# CODEFORGE_E2E=1 go test ./pkg/server/ -run TestE2ELiveLLM -v -timeout 900s
```

未设置 `CODEFORGE_E2E=1` 时自动跳过，因此不影响日常 `go test ./...`。

### Makefile 快捷目标

| 目标 | 说明 |
|---|---|
| `make run` | 构建并启动服务 |
| `make stop` | 停止正在运行的服务（端口被占用时先执行） |
| `make restart` | 停止旧实例后重新构建并启动 |
| `make test` | 全部测试（Go + 前端渲染器 + pytest） |
| `make test-go` | Go 单元 / 集成测试 |
| `make test-web` | 前端 Markdown 渲染器测试（Node，零依赖，不消耗额度） |
| `make test-llm` | LLM 端点协议测试（pytest） |
| `make test-e2e` | 真实 LLM 端到端（Agent + 工具 + HITL） |

---

## 技术选型

- Go 1.25+（本机验证于 go1.27.0），`CGO_ENABLED=0`
- `github.com/gorilla/websocket`、`gopkg.in/yaml.v3`、`github.com/tetratelabs/wazero`（全部纯 Go）
- 前端：原生 HTML / CSS / JS，零构建链
