# AGENTS.md — CodeForge-Go 工作约定

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
> `config/codeforge.run`，但仍不要把它们当垃圾桶留在工作区。
