# CodeForge-Go 跨平台构建脚本
# 所有目标均以 CGO_ENABLED=0 构建，产出纯静态单二进制。

BINARY  := codeforge
CMD     := ./cmd/agent
LDFLAGS := -s -w
GOFLAGS := -trimpath
PYTHON  ?= python
# 前端渲染器测试用；本机若 node 不在 PATH，可指定：
#   make test-web NODE=C:/Users/26536/.workbuddy-ai/binaries/node/versions/22.22.2-2/node.exe
NODE    ?= node

# Windows 上产物带 .exe 后缀，与 cf.cmd / README 中的用法保持一致
# （go build -o bin/codeforge 不会自动补 .exe，若不统一会出现
#   bin/codeforge 与 bin/codeforge.exe 两个本机产物各自变旧的问题）。
ifeq ($(OS),Windows_NT)
BINEXT  := .exe
KILL    := taskkill //F //IM $(BINARY).exe
else
BINEXT  :=
KILL    := pkill -f $(BINARY)
endif

LOCALBIN := bin/$(BINARY)$(BINEXT)

.PHONY: all build windows linux-amd64 linux-arm64 android-arm64 all-platforms \
        run stop restart test test-go test-web test-llm test-e2e vet fmt clean

all: build

## 本机平台构建
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(LOCALBIN) $(CMD)

## 构建并启动服务（自动读取系统环境变量里的 CODEFORGE_API_KEY 与 config/local.yaml，
## Windows 下为 bin/codeforge.exe）
##
## 端口由全局配置 config/default.yaml 决定，不做自动换端口；
## 端口被占用时请先 `make stop` 关闭旧实例，或改配置。
## 另注：-config 相对当前工作目录解析，因此必须在项目根目录执行。
run: build
	./$(LOCALBIN) -config config

## 停止正在运行的服务（端口被占用时先执行）
stop:
	-$(KILL)

## 停止旧实例后重新构建并启动
restart: stop build
	./$(LOCALBIN) -config config

## Windows x86_64
windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-windows-amd64.exe $(CMD)

## Linux x86_64
linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-linux-amd64 $(CMD)

## Android Termux arm64
android-arm64:
	CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY)-android-arm64 $(CMD)

## 全部平台（默认不含 linux-arm64，见上）
all-platforms: windows linux-amd64 android-arm64

## 全部测试（Go 单测 / 集成 + 前端渲染器 + LLM 端点 pytest）
test: test-go test-web test-llm

## Go 单元测试与集成测试
test-go:
	go test ./...

## 前端回归测试（纯 Node，零依赖，无需浏览器；不消耗任何额度）
##
## ⚠️ 这里**必须列全** web/test/ 下的每一个测试文件。早先只跑 render_md.test.js，
## 把 attachments.test.js 漏在外面，于是「附件脚手架缺依赖、重复提交没被拦住」
## 这一整类缺陷在 CI 上全程绿灯 —— 绿灯并不代表前端全绿。
## 新增前端测试时请同步加到这一行。
test-web:
	$(NODE) web/test/render_md.test.js
	$(NODE) web/test/attachments.test.js

## LLM 端点协议测试（pytest，读取系统环境变量 CODEFORGE_API_KEY）
test-llm:
	$(PYTHON) -m pytest tests/ -v

## 真实 LLM 端到端测试（Agent 循环 + 工具调用 + HITL 审批）
test-e2e:
	CODEFORGE_E2E=1 go test ./pkg/server/ -run TestE2ELiveLLM -v -timeout 600s

vet:
	go vet ./...

fmt:
	gofmt -w .

clean:
	rm -rf bin
