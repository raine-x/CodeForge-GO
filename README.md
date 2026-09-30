# CodeForge-Go

Go 编写的跨平台 Web Agent。在浏览器里对话，Agent 在你的项目目录里读写文件、跑命令、调模型。

单文件静态二进制，零运行时依赖，Windows / Linux / Android Termux 通用。

**界面就是一个网页** —— 这不只是形态选择，它让同一份程序能跑在**没有图形界面的服务器**上：SSH 进去启动，用手机或笔记本的浏览器打开就能用；在 Android Termux 上也是同一套，没有任何桌面环境依赖。

---

## 特性

### 浏览器 UI = 无 GUI 环境可用

界面是一个网页，不依赖 X11/Wayland/桌面环境。同一份二进制在这些场景下都能用：

| 场景 | 怎么用 |
|---|---|
| **无 GUI 服务器** | SSH 上去启动，`-no-open` 免得尝试拉浏览器；用本地浏览器访问 `http://<服务器地址>:8420` |
| **Android Termux** | 手机上直接跑，浏览器开 `http://127.0.0.1:8420` |
| **容器 / NAS / 树莓派** | 同服务器，暴露端口即可 |
| **Windows / 桌面 Linux** | 启动后自动打开浏览器 |

没有 Electron、没有前端构建步骤、没有 npm 依赖 —— `web/dist/` 里的文件是手写源码，`go:embed` 直接打进二进制。**整个界面跟着二进制走，部署只需要拷贝一个文件。**

默认只监听 `127.0.0.1`（本机回环），不对外暴露。需要从别的机器访问时改 `config/default.yaml`：

```yaml
server:
  host: "0.0.0.0"   # 监听所有网卡
  port: 8420
```

> ⚠️ 改成 `0.0.0.0` 等于把 Agent 的读写权限暴露到网络上。服务自带访问令牌做防护，但**务必只在你信任的网络里这么做**，且不要直接暴露到公网。

会话列表、模型管理、设置、审批卡片、diff 预览都在同一个界面里。

### 专为 Termux 与低配环境做的适配

不是「顺便能跑」，是有专门处理：

- **shell 探测**：Termux 上优先用 `$PREFIX/bin/bash`（Android 没有 `/bin/bash`），回退 `/bin/sh`。
- **存储授权**：主动跑 `termux-setup-storage` 申请权限，授权后 `~/storage/shared` 软链可用。
- **目录选择器**：没有原生文件夹对话框时用内置浏览器式选择器，起始目录挂在 `~`（一定列得出来，不像 `/storage/shared` 未授权时是空的）。
- **进程组管理**：Android 的进程终止行为与桌面 Linux 不同，单独适配。
- **内存软上限**：`GOMEMLIMIT` 按物理内存 25% 设定 —— 手机上给的内存配额通常远小于桌面，靠这个防止被系统 OOM killer 杀掉。

### 低内存占用

启动后常驻约 25 MB（Windows amd64 空载实测 Working Set）。几个关键设计：

- **软内存上限**：`GOMEMLIMIT` 按物理内存的 25% 自动设定，夹逼在 256 MiB ~ 2 GiB。超限时 Go GC 更积极，而不是等系统 OOM。
  可用 `CODEFORGE_GOMEMLIMIT` 覆盖（支持 `2GiB` / `512MiB` / 纯字节三种写法，`0` 表示关闭）。容器里读的是**物理内存而非 cgroup 配额**，会偏大，那种场景靠这个覆盖。
- **上下文主动压缩**：每轮请求前检查 token 占用，超预算先压缩历史再发送。
- **工具结果限幅**：大结果由工具分页，Executor 只做兜底截断，且截断产出仍是合法 JSON。
- **撤销栈有字节预算**：单条超限落盘、总量有上限、丢弃时明确告知。

### 多会话并发

同一个项目里可以同时跑多个会话，互不干扰：

- **运行表按会话分槽**：每个会话各有自己的取消句柄，发消息和打断只作用于本会话。
- **撤销栈按会话分桶**：A 的撤销撤不到 B 的写入，A 的大文件不会挤掉 B 的快照。
- **阅读登记按会话隔离**：A 读过的文件不会让 B 获得「凭记忆改写」的资格。
- **运行中转向只对当前会话生效**：不会被「恰好有个后台任务在跑」改道到别的会话。
- **不跨会话打断**：在 B 按打断不会停掉后台正在跑的 A。

> **当前的边界**：并发在**同一工作区内**完全成立。跨工作区并发还不支持 ——
> 工作区是进程级的，切它会改掉正在跑的那些会话所看到的工作区，所以有任务在跑时
> 跨项目切换会被挡并给出提示。把工作区下沉到每个会话是进行中的工作。

### 跨平台静态编译

`CGO_ENABLED=0`，产物导入表为空 —— 干净机器上直接跑，不需要装任何运行库。

| 平台 | 产物 | 大小 |
|---|---|---|
| Windows amd64 | `codeforge.exe` | 16.3 MB |
| Linux amd64 | `codeforge` | 15.9 MB |
| Android arm64（Termux） | `codeforge` | 16.4 MB |

### 内置工具

文件：`read_file` `write_file` `edit_file` `delete_file` `list_dir` `search_files` `find_files`
命令：`run_command`
协作：`delegate_subagents`（多智能体） `goal_verify`（目标模式强约束校验）
其它：`todo_write` `save_memory` `create_skill` `web_fetch` `web_search`

### 文件操作的安全约束

这几条是硬性的，改不动：

- **先读后写**：没读过的文件不允许凭记忆改写。读取之后若被外部改动，指纹比对会发现并拒绝。
- **CAS 写入**：「读当前 → 比对 → 写」在同一把按路径的锁内完成，并发写不会静默互相覆盖。
- **原子落盘**：同目录临时文件 + rename。进程写一半被杀不会留半截文件。
- **工作区围栏**：路径必须落在工作区内。「区内软链指向区外、且目标尚不存在」这类绕过会被逐级向上解析挡住。
- **原子级撤销**：写前快照，按会话分桶，可逐字节还原。

### 人工审批（HITL）

策略判定为 `Ask` 的操作会挂起，在界面上等你的决定。审批有独立超时（默认 15 分钟），与工具执行超时分开 —— 「用户拒绝」和「没人管」在界面上的说法不同。

### 其它

- **断点续跑**：任务被打断后保留未完成轮次，可从断点重试或编辑重发。
- **系统通知**：任务完成时按需发系统通知，带可见性判断与节流。
- **子智能体**：把任务派给多个子智能体并行处理，进度实时回传。
- **目标模式**：完成前必须通过 `goal_verify` 校验，不通过就继续。
- **插件机制**：MCP / 远程能力可适配成标准工具接入。

---

## 快速开始

### 1. 构建

```bash
# 本机（Windows 产物是 bin/codeforge.exe —— go build -o bin/codeforge 不会自动补
# .exe，请显式写全后缀，否则会与 Makefile / cf.cmd 用的文件名不一致）
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/codeforge.exe ./cmd/agent

# 交叉编译三件
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/codeforge-windows-amd64.exe ./cmd/agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/codeforge-linux-amd64      ./cmd/agent
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o bin/codeforge-android-arm64  ./cmd/agent
```

或用 Makefile：`make build` / `make all-platforms`（本机构建会自动带正确后缀）。

### 2. 配置 API Key

**密钥只从系统环境变量读**，不读任何密钥文件。这样密钥不会躺在项目目录里被同步盘、备份脚本随手带走。

**Linux / macOS / Termux**（写进 shell 配置持久生效）：

```bash
echo 'export CODEFORGE_API_KEY=sk-...' >> ~/.bashrc
source ~/.bashrc
```

**Windows**（`setx` 对新终端生效）：

```powershell
setx CODEFORGE_API_KEY "sk-..."
```

### 配置文件里引用环境变量

不想每次都 export？**在 YAML 里写 `${变量名}`**，值在加载时从环境取：

```yaml
# config/providers.yaml
providers:
  - id: my-gateway
    name: 某网关
    base_url: ${GATEWAY_URL}/v1     # ← 变量引用，文件里没有地址
    protocol: openai
    key_source: plain
    key_value: ${GATEWAY_KEY}       # ← 变量引用，文件里没有密钥
```

`config/models.yaml` 与 `config/local.yaml` 同样支持：

```yaml
# config/local.yaml
llm:
  api_key: ${MY_KEY}
  base_url: ${MY_BASE_URL}
  model: gpt-4o
```

**支持 `${NAME}` 与 `$NAME` 两种写法**；`\${NAME}` 取字面量。展开只发生在内存里，**不会改写文件**。

**变量没设时怎么办**：按空处理，并在启动日志里点名：

```
WARN  供应商库 config/providers.yaml 引用了未设置的环境变量：GATEWAY_KEY（这些值已按空处理；…）
```

这样配了十项错一项时，其余九项照样能用；而且日志会直接告诉你少了哪个变量 —— 否则症状是「明明配了却连不上」，错误信息指向 401，完全看不出是变量没设。

### 变量名优先级

若没在 YAML 里写 `${...}`，程序按 `config/config.go` 的 `applyEnvFallback` 依次尝试：

| 变量名 | 说明 |
|---|---|
| `CODEFORGE_API_KEY` | **推荐**，所有 provider 通用，优先级最高 |
| `ANTHROPIC_API_KEY` | `provider: anthropic` 时的次选 |
| `OPENAI_API_KEY` | `provider: openai` / `custom` 时的次选 |
| `LLM_API_KEY` | 最后一档兜底（旧代码的默认名） |

> 如果 `config/local.yaml` 里显式写了 `llm.api_key`（含 `${…}` 展开后的值），**它优先于上面这条链**。

**非敏感配置**（模型名、协议等）直接在 YAML 里写，或用界面上的「设置」页改 —— 那会写进 `config/models.yaml` 与 `config/providers.yaml`（两个文件都在 `.gitignore` 里）。

### 3. 启动

```bash
# 方式一：启动器（内部自动切到项目根目录，与当前目录无关）
./cf.cmd                    # Windows
./cf-termux.sh              # Android Termux

# 方式二：直接运行 —— 两个路径都给绝对值，从哪启动都一样
./bin/codeforge -config /path/to/project/config -workdir /path/to/project
./bin/codeforge.exe -config "C:\path\to\project\config" -workdir "C:\path\to\project"

# 方式三：Makefile（构建 + 启动，会自动切目录）
make run
```

启动后浏览器会自动打开 <http://127.0.0.1:8420>。

**无 GUI 环境下务必加 `-no-open`** —— 否则程序会尝试调用 `xdg-open` / 浏览器并失败（多数情况下只是报错，不影响服务，但日志里会有噪音）：

```bash
# 无 GUI 服务器
./bin/codeforge -no-open -config /srv/myproject/config -workdir /srv/myproject
```

然后在本地浏览器访问 `http://<服务器地址>:8420`。

### 4. 选择工作区

界面右上角点「选择工作区」，或用参数直接指定：

```bash
./bin/codeforge -workdir /path/to/project
```

**工作区必须是绝对路径** —— 相对路径没有意义（工作区没法相对自己），放任相对路径会被按进程当前目录解析，换个启动方式工作区就变了。

目录不存在时会按「未选择工作区」处理，不会静默设成工作目录。

### 5. 开始对话

在工作区已选中的前提下，直接在输入框里说话。

- **运行中想换方向**：直接打字发送，指令会在下一个步骤边界并入当前任务（只对当前会话生效）。
- **运行中想停**：输入框清空，按 `Esc` 或点发送按钮。
- **切换会话**：点侧栏的会话卡片。同一项目里的会话可以并行跑。

---

## 命令行参数

```
codeforge [start]  [-config 目录] [-workdir 目录] [-no-open]   启动服务（默认）
codeforge stop     [-config 目录]                              停止运行中的服务
codeforge restart  [-config 目录] [-workdir 目录] [-no-open]   重启服务
```

| 参数 | 说明 |
|---|---|
| `-config 目录` | 配置目录，默认 `config` |
| `-workdir 目录` | Agent 工作目录（必须绝对路径），默认当前目录 |
| `-no-open` | 不自动打开浏览器 |
| `-continue` | 启动后回到该工作区最近更新的那条会话 |
| `-resume <ID>` | 启动后回到指定会话：会话 ID / ID 前缀 / 会话标题 / `last` |

`-resume` 指定的会话若属于别的工作区，会先切工作区再载入 —— 在一个项目里回放另一个项目的对话，模型下一步改的就是错项目的文件。

---

## 各平台启动

### 三条通用规则

1. **`-config` 与 `-workdir` 都给绝对路径**。给绝对值之后从哪个目录启动都一样，不必切 CWD（用启动器或 `make run` 则是自动帮你切）。
2. **工作区必须是绝对路径** —— 相对路径没有意义（工作区没法相对自己），放任它会被按进程当前目录解析，换个启动方式工作区就变了。
3. **配置的工作区不存在或不是目录**，按未选择工作区处理，不会静默设成工作目录。

另外：**没有图形环境时加 `-no-open`**（Termux、无 GUI 服务器、容器内都需要）。

### 无 GUI 服务器（Linux / BSD / 容器 / NAS）

这是这个程序被设计成网页界面的主要理由。

```bash
# 上传到服务器
scp bin/codeforge-linux-amd64 user@server:/usr/local/bin/codeforge

# 启动（-no-open 免得它尝试拉浏览器；两个路径都给绝对值，从哪启动都一样）
./codeforge -no-open -config /srv/myproject/config -workdir /srv/myproject
```

`-config` 与 `-workdir` 都给绝对路径，从哪个目录启动都一样。密钥走系统环境变量（`CODEFORGE_API_KEY`），在本地 shell 里 `export` 即可。

然后：

1. 默认只监听 `127.0.0.1`，此时只有服务器本机能访问 —— 用 SSH 端口转发最省事，不用改配置：

   ```bash
   # 在你自己的机器上执行
   ssh -L 8420:127.0.0.1:8420 user@server
   ```

   然后浏览器开 <http://127.0.0.1:8420>。

2. 需要从别的机器直接访问时，把 `config/default.yaml` 的 `server.host` 改成 `0.0.0.0`，
   并确认防火墙放行 8420 端口。服务启动时会生成随机访问令牌并通过 HttpOnly Cookie 下发，
   API 与 WebSocket 都会校验它，所以没有匿名访问。

   > ⚠️ 改 `0.0.0.0` 等于把 Agent 的文件读写能力暴露到网络上。
   > 令牌能挡住未授权访问，但请**只在你信任的网络里这么做**，并优先用 SSH 端口转发。

后台常驻可以用 systemd：

```ini
[Unit]
Description=CodeForge
After=network.target

[Service]
ExecStart=/usr/local/bin/codeforge -no-open -config /srv/myproject/config -workdir /srv/myproject
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

用 systemd 时**必须**用 `Environment=` 或 `EnvironmentFile=` 把密钥传进去 —— 服务没有 TTY，读不到你在交互式 shell 里 `export` 的东西：

```ini
[Unit]
Description=CodeForge
After=network.target

[Service]
ExecStart=/usr/local/bin/codeforge -no-open -config /srv/myproject/config -workdir /srv/myproject
EnvironmentFile=/etc/codeforge/env
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`/etc/codeforge/env` 里放 `CODEFORGE_API_KEY=sk-...`，并设权限 `chmod 600` + `chown root:root`。放在 `/etc` 而不是项目目录，是为了让它跟着「配置」走而不是跟着「代码」走。

`stop` / `restart` 子命令同样可用。

### Windows x64

双击 `cf.cmd`，或：

```powershell
.\bin\codeforge.exe -config config
```

### Linux x86_64 / arm64

```bash
chmod +x bin/codeforge
./bin/codeforge -config /path/to/project/config -workdir /path/to/project
```

静态链接，无动态库依赖。把 `-config` / `-workdir` 给成绝对路径，就不必关心 CWD 在哪。

### Android Termux arm64

手机上没有桌面环境，界面全靠浏览器 —— 这也是把 UI 做成网页的直接好处。

```bash
pkg install golang
git clone <repo> && cd codeforge-go
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o codeforge ./cmd/agent
chmod +x codeforge
./codeforge -no-open -config config
```

`-no-open` 在 Termux 上是必需的 —— 否则它会尝试拉起系统浏览器，多数情况下会失败。

启动后用手机浏览器打开 <http://127.0.0.1:8420>。若想用外部网络访问同一台服务，按「无 GUI 服务器」那节改 `server.host`。

选工作区时 Termux 权限需要注意：

- 默认起始目录是 `~`，一定能列出内容。
- 需要访问外部存储时先跑 `termux-setup-storage` 申请授权，之后 `~/storage/shared` 软链可用。
- 若 Android 版本上目录列表为空，多半是存储权限没授 —— 授权后重试。
- shell 探测会优先找 `$PREFIX/bin/bash`（Android 没有 `/bin/bash`），回退 `/bin/sh`。

### 最小化部署

只需要二进制 + `config/default.yaml` + `config/models.yaml`。`config/local.yaml`、`config/providers.yaml`、`.codeforge/` 都会在首次运行时自动生成。密钥不进文件，走系统环境变量。

**没有构建工具链也能用** —— 二进制是静态的，拷过去直接跑。

---

## 配置与密钥

### 分层与写入者

| 文件 | 谁写 | 说明 |
|---|---|---|
| `config/default.yaml` | 人工维护 | 随仓库提供 |
| `config/models.yaml` | 界面「设置」 | 模型库。字段可写 `${VAR}` 引用环境变量 |
| `config/providers.yaml` | 界面「设置」 | 供应商库。字段可写 `${VAR}` 引用环境变量 |
| `config/local.yaml` | 人工编辑 | 本地覆盖，**禁止提交** |
| **系统环境变量** | 人工设置 | **密钥的落点**：`CODEFORGE_API_KEY`，或 YAML 里 `${...}` 引用它 |
| `.codeforge/` | 程序 | 会话、审计日志、撤销副本、记忆 |

### 密钥在哪

**首选：系统环境变量。** 程序不读密钥文件 —— 文件会躺在项目目录里被同步盘、备份脚本、
`docker cp` 随手带走，而环境变量不会。

三种写法，效果相同：

```yaml
# 1) 显式引用（推荐，配置文件里自解释）
key_value: ${MY_GATEWAY_KEY}

# 2) 只记变量名，请求时才去读（界面上的「环境变量」选项）
key_source: env
key_name: MY_GATEWAY_KEY

# 3) 什么都不配，走内置优先级链
#    CODEFORGE_API_KEY → 厂商专用名 → LLM_API_KEY
```

**次选：界面「设置」页直接填。** 方便但会写成明文，存在 `config/providers.yaml` 里。

**兜底：`config/local.yaml` 的 `llm.api_key`。** 优先级最高 —— 写了就压过上面两者。

三者可以并存。想收敛到一处，把另两处清空即可。

### 密钥是硬约束

`config/models.yaml`、`config/providers.yaml`、`config/local.yaml` **全部在 `.gitignore` 里**，
且是按**文件名通配**（`*models.yaml`）而非路径 —— 配置目录改名也挡得住。

`.gitignore` 还会按**文件名**忽略含 `secret` / `credential` / `api_key` / `apikey` 的文件 ——
代价是正常源码里带这些词的文件也会被静默忽略。写代码时避开这些词（凭据解析那套叫 `config/resolve_llm.go`）。

---

## 安全模型

三级策略：**Allow / Ask / Deny**。

- 规则在 `config/default.yaml` 的 `security.rules` 里，有序匹配。
- **内置危险命令黑名单优先级最高**：`rm -rf /` 之类直接 Deny，规则表改不动。
- 条件解析失败按**拒绝**处理，不是跳过。
- 命中 `Ask` 的操作走人工审批，审批渠道在界面上。
- 每次工具调用都写审计日志（JSONL，超 1 MiB 轮转，保留 3 份）。

`agent.hidden_tools`（控制发给 LLM 的工具表）与 `security.Policy`（控制是否放行）是**刻意分离**的：隐藏不等于禁止。不要合并成一个开关。

---

## 子智能体

`delegate_subagents` 把任务派给多个子智能体并行处理。子智能体有独立的策略上限（三处落点同步：Agent / 调度器 / 工具 schema）。

子智能体的操作**全部进审计日志**（历史上曾漏配，导致子智能体操作不留痕）。

## 目标模式

`goal_verify` 在任务完成前做强制校验。不通过就继续做，不会静默放过。审查者的 audit 传真 logger。

---

## 打断之后怎么接着跑

任务被打断时保留未完成轮次：

- 界面上的重试圆环：点击从原提问重新开始（保留已产生的上下文）。
- 编辑重发：改写某条历史提问后重发，服务端截断到那条消息并重跑。
- 文件回滚：编辑重发时可连带回滚该消息之后的文件改动。

断点状态在检查点面板里可见。

---

## 测试

### Go 单元 / 集成测试

```bash
go test ./...
```

e2e 默认 Skip，需 `CODEFORGE_E2E=1` 且配置真实 Key。

### 前端渲染器测试（Node，零依赖）

```bash
node web/test/render_md.test.js
node web/test/attachments.test.js
```

改 `ui.js` 后**必须**跑这两个。测试里有对源码文本的断言，改函数名或结构要同步。

### 提交前自检

```bash
gofmt -l .
go vet ./...
go test ./...
node web/test/render_md.test.js
node web/test/attachments.test.js
# 三平台编译
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/agent
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/agent
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o bin/ ./cmd/agent
```

`go test -race` 需要 C 编译器（`-race` 依赖 cgo）。没装 gcc 时不可用 —— 这也是为什么并发相关的护栏主要是**结构性断言**（解析源码确认形状）而不是竞态检测。

有 Makefile 的话 `make test` / `make test-web` 可以一次跑完。

---

## 目录结构

```
cmd/agent/          入口：flag 解析、进程生命周期、装配
pkg/agent/          编排：ReAct 循环、历史、压缩、检查点
pkg/tools/          工具接口、注册、执行器（含策略判定与审批）
pkg/tools/builtin/  内置工具实现 + 会话守卫（围栏/指纹/撤销栈）
pkg/tools/plugins/  插件驱动（MCP / 远程能力适配成工具）
pkg/backend/        执行环境抽象（纯 IO 后端，本机实现）
pkg/security/       策略判定与审计日志
pkg/server/         HTTP + WebSocket 服务
pkg/llm/            LLM 适配器（SSE 流式）
pkg/store/          SQLite 持久化
pkg/platform/       平台差异（shell、进程组、路径）
pkg/errs/           错误分类
pkg/logx/           结构化日志
web/dist/           前端（手写单文件，无构建步骤）
config/             配置（见「配置与密钥」）
```

---

## 技术选型

| 方面 | 选择 | 理由 |
|---|---|---|
| 语言 | Go | 单文件静态编译，跨平台，内存可控 |
| 存储 | SQLite（`modernc.org/sqlite`，纯 Go） | 零外部依赖，`CGO_ENABLED=0` 也能静态编译 |
| 前端 | 手写单文件 JS | 无构建步骤 = 无 node_modules、无版本漂移 |
| 依赖 | 极少 | 静态编译与产物体积是产品目标 |

---

## 许可

见仓库。
