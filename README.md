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
# 交叉编译
make windows linux-amd64 linux-arm64 android-arm64
```

### 2. 配置 API Key

程序启动时会**自动加载当前目录的 `.env`**（不覆盖真实环境变量），并自动生成 `config/local.yaml` 模板。

> **模型统一在「设置 → 模型」里管理**（服务端模型库 `config/models.yaml`，首次启动会把
> 当前生效模型自动种入列表）。对话框旁的模型名只是「当前生效项」的展示，列表的增删改查
> 一律走设置页；密钥明文只存在服务端（`models.yaml` / `local.yaml`，均 0600 且被 gitignore），
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

也可以启动后在界面「设置」中填写 —— 保存时会自动写入 `config/local.yaml`，前端只显示掩码。

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

> **工作区选择会记住**：在界面选择工作区后，绝对路径自动写回 `config/local.yaml` 的
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
| Linux arm64 | `bin/codeforge-linux-arm64` | 树莓派 / ARM 云主机 |
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
chmod +x codeforge-linux-amd64        # arm64 换成 codeforge-linux-arm64
./codeforge-linux-amd64 -config config
```

- **无桌面环境的服务器**：加上 `-no-open`（否则只会在日志里提示找不到浏览器），然后从浏览器访问
  `http://<服务器IP>:8420/` —— 记得先把 `server.host` 改成 `0.0.0.0`。
- 自动开浏览器依次尝试 `xdg-open` → `open`（macOS），都没有时会提示手动访问。
- `codeforge stop` / `restart` 在各平台通用：优先向实例发带令牌的 `POST /api/shutdown` 优雅关闭，
  失败再按 `codeforge.run` 里的 PID 强制结束（PID 失效时按端口定位，POSIX 上依次尝试 `lsof` / `ss`）。

### Android Termux arm64

```bash
pkg install -y termux-tools           # 可选：提供 termux-open-url，用于自动拉起浏览器
chmod +x codeforge-android-arm64
./codeforge-android-arm64 -config config
```

- `GOOS=android` 的产物专为 Termux 编译；`codeforge-linux-arm64` 在 Termux 里同样可以运行。
- 在 Termux 中平台名会被识别为 `termux`（依据 `$PREFIX` 是否含 `com.termux`）。
- 想关掉自动开浏览器用 `-no-open`。

### 最小化部署

只需要这三样，放到目标机器的同一个目录下即可：

```
/opt/codeforge/
├── codeforge-linux-amd64     # 对应平台的二进制
├── config/                   # default.yaml + local.yaml（建议整目录拷贝）
└── .env                      # 只放 API Key 即可
```

启动后 API Key 也可以直接在界面「设置」里填，会写入 `config/local.yaml`（前端只显示掩码）。

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

### 被忽略的内容（`.gitignore`）

| 模式 | 说明 |
|---|---|
| `config/local.yaml`、`config/*.local.yaml` | 本地配置覆盖（当前生效模型）；界面保存密钥时写入 |
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
- **当前生效模型的密钥在设置页可直接查看**：`/api/models/list` 对「已应用/生效」的那一条
  额外附带 `key_plain` 明文（仅此一条），编辑该模型时密钥框默认掩码显示、点「眼睛」可切明文；
  其余模型仍只有 `key_set`，前端不接触其明文。
- **「测试连接」**（`/api/models/test`）：默认用表单里填的密钥探测上游；表单没给 key 时
  回退到模型库当前模型条目（与「应用」同源的密钥解析），保证测试与对话一致。
  探测用 `max_tokens=1` 的最小请求，遇 5xx/429 等上游瞬时故障会原样回报，并附带提示
  「实际对话使用的生效配置可能与此不同」。

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

### 4. 改完必须重新编译

前端属于 `go:embed` 资源，`ui.js` / `app.css` 的改动**不会热更新**，必须重新 `go build` 才会生效。

```bash
node web/test/render_md.test.js      # 或 make test-web —— 先跑回归测试
make build && cf restart             # 再重编译并重启
```

### 5. 目前未支持的语法

图片、嵌套列表、任务列表（`- [ ]`）尚未实现，遇到时降级为普通文本 / 单层列表。

---

## 测试

### Go 单元 / 集成测试

```bash
go vet ./...
go test ./...
```

覆盖：Unified Diff 生成、安全策略三级判定、危险命令拦截、Cookie 鉴权、WebSocket 端到端 Agent 事件流。

### 前端渲染器测试（Node，零依赖）

`web/dist/ui.js` 里的 `renderMD()` 是手写的零依赖 Markdown 渲染器，改它很容易悄悄弄坏别的语法，
所以单独配了一层回归测试 —— **纯 Node 运行，不需要浏览器、不消耗任何额度**：

```bash
node web/test/render_md.test.js       # 或 make test-web
```

它不加载整个 `ui.js`（那个文件加载时会访问 DOM），而是从源码里按花括号配对截出
`escapeHtml` / `renderMD` / `toolLabel` 三个纯函数注入调用，覆盖：

- 基础语法：标题 / 粗斜体 / 行内代码 / 链接 / 引用 / 分隔线 / 列表 / 段内换行 / HTML 转义防注入
- 表格：三种对齐、列数与行数、不被包进 `<p>`、与相邻标题各自成块、缺少分隔行时不误判
- 公式：`$…$` `$$…$$` `\(…\)` `\[…\]` 四种写法，以及 `$a*b*c$` 不被吃成斜体、
  `$x_1$` 下标不被吞、**「价格 $5 到 $10」不被误判成公式**、代码块内公式不处理
- 工具卡片文案 `toolLabel()`：文件名只取末段、`list_dir` / `search_files` 不带目标、
  缺参数时降级为纯短语、未知工具回退为「调用了 &lt;name&gt;」、超长命令与文件名截断
- `app.css` 样式契约：`.msg-assistant.md` 必须重置 `white-space`（见「前端渲染注意事项」一节）

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
