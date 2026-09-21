# AGENTS.md — CodeForge-Go 工作约定

## 完成后的编译

每次修改完成并验证后，默认进行全平台编译，除非用户明确指明不编译。
产物放入 `bin/`：Windows amd64、Linux amd64、Android arm64；同时更新本机入口 `bin/codeforge.exe`。
`bin/codeforge-linux-arm64` **不在默认编译范围内**（2026-09-20 起）：需要时用 `make linux-arm64` 单独出，或明确点名要求编译。
使用 `CGO_ENABLED=0`、`-trimpath` 和 `-ldflags "-s -w"`。编译失败须如实说明，不得宣称已完成编译。

## 测试用例 / 临时文件 / 断言的位置

所有测试用例、临时文件、断言都不许散落在仓库里，必须放到约定的位置：

| 类型 | 位置 | 说明 |
| --- | --- | --- |
| 临时脚本、中间产物、调试输出、抓包/验证脚本 | `test/`（仓库根） | 用完即删；不要留在仓库根或源码目录 |
| 前端测试与断言 | `web/test/` | 如 `web/test/render_md.test.js` |
| Go 测试 | **与源码同目录**（`pkg/**/xxx_test.go`、`config/xxx_test.go`） | 见下方说明 |

### 为什么 Go 测试例外（同目录）

Go 的硬约束：要测试**未导出**函数（如 `notifySkipReason`、`sameFile`、`pathWithin`），
用例必须与源码**同包同目录**。搬到顶层 `test/` 包只能做黑盒测试、只覆盖导出符号，
会丢掉内部逻辑的回归保护。

本仓库既有 24 个 `*_test.go` 均为同目录，新增测试请沿用：

```
config/config_test.go
pkg/agent/subagent_test.go
pkg/server/notify_test.go
pkg/tools/builtin/workspace_test.go
...
```

### 临时文件

仓库根目录只允许放项目文件（`go.mod`、`Makefile`、`README.md`、`cf.cmd`、
`.gitignore`、`.env*`、`AGENTS.md`、`pytest.ini`）。
调试用的 `.py` / `.js` / `.log` / `.tmp` / `.bak` 一律放 `test/`，并在收尾时清理。

> `.gitignore` 已忽略 `*.tmp`、`*.log`、`config/local.yaml`、`config/models.yaml`、
> `config/providers.yaml`，但仍不要把它们当垃圾桶留在工作区。

### 配置分层：一个文件一个写入者

合并次序 `default.yaml → local.yaml → ~/.codeforge/state.yaml`（后者覆盖前者）：

| 文件 | 写入者 |
|---|---|
| `config/default.yaml`、`config/plugins.yaml` | 代码 / Git |
| `config/local.yaml` | 用户手工编辑，**程序只读** |
| `config/models.yaml`、`config/providers.yaml` | 设置页 |
| `~/.codeforge/state.yaml` | 程序（`Config.SaveState()`，运行状态唯一写盘入口） |
| `~/.codeforge/run.json` | 程序（当前进程，退出即删） |
| `~/.codeforge/data.db` | 程序（会话/记忆，跨工作区共享） |

**新增「界面可改」的配置项时，写进 `config/state.go` 的 `State` 投影，不要调
`Config.Save()`。** 后者序列化整个 Config，会把插件目录和安全规则这类切片整体抄进
目标文件；yaml 对切片是替换而非合并，于是默认值以后新增的条目会被旧副本静默吃掉
（`security.rules` 少 `todo_write`、`web_*` 就是这样丢的）。投影刻意不含这些字段，
所以不可能复发。运行状态文件的位置可用 `CODEFORGE_STATE` 覆盖 —— 测试必须覆盖它，
否则会写真实用户目录（各包的 `TestMain` 已统一处理）。
