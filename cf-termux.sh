#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# CodeForge-Go —— Termux 快捷启动（cf-termux）
#
# 为什么需要它：
#   桌面端 cf.cmd 是 Windows 批处理，Termux 用不了；而 Makefile 的 android-arm64
#   目标是**从 PC 交叉编译**（GOOS=android GOARCH=arm64）。在手机上是本机编译：
#   Termux 的 `go env` 本来就报 android/<本机架构>，所以这里刻意不覆盖
#   GOOS/GOARCH —— 覆盖成别的值会得到一个 Termux 跑不起来的产物。
#
#   另一个理由与 cf.cmd 相同：-config 相对**当前工作目录**解析，
#   不是相对可执行文件。从别处调用会静默退回内置默认值（provider=anthropic）。
#   所以脚本开头无条件 cd 到项目根。
#
# 用法（在 Termux 中）：
#   bash cf-termux.sh              拉最新代码 → 编译 → 前台启动（默认）
#   bash cf-termux.sh --no-pull    不拉取，只编译并启动本地代码
#   bash cf-termux.sh --no-run     拉取并编译，但不启动
#   bash cf-termux.sh --test       启动前先跑一遍 go test ./...
#   bash cf-termux.sh -- -no-open  把后面的参数传给程序（-no-open / -workdir 等）
#
# 首次使用需装依赖：
#   pkg install golang git
#
# 停止服务：Ctrl-C，或 bin/codeforge stop
# ---------------------------------------------------------------------------
set -euo pipefail

cd "$(dirname "$0")"

BIN=bin/codeforge
DO_PULL=1
DO_RUN=1
DO_TEST=0
APP_ARGS=()

usage() {
	sed -n '3,25p' "$0" | sed 's/^# \{0,1\}//'
}

die() {
	printf '\n✗ %s\n' "$*" >&2
	exit 1
}

ok() {
	printf '✓ %s\n' "$*"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--no-pull) DO_PULL=0 ;;
	--no-run)  DO_RUN=0 ;;
	--test)    DO_TEST=1 ;;
	-h | --help)
		usage
		exit 0
		;;
	--)
		shift
		APP_ARGS=("$@")
		break
		;;
	*)
		printf '未知参数：%s\n要传给程序请用 -- 分隔，例如：bash cf-termux.sh -- -no-open\n' "$1" >&2
		exit 2
		;;
	esac
	shift
done

[ -f go.mod ] || die "当前目录不是项目根（缺 go.mod）"
command -v go >/dev/null 2>&1 || die "没装 Go：pkg install golang"

# go.mod 要求 go >= X。Termux 的 golang 包若偏旧，Go 会按 GOTOOLCHAIN 规则
# 尝试联网下载工具链，手机网络不一定通 —— 与其卡住不如提前说清楚。
# GOTOOLCHAIN=local 是关键：不加的话，光是执行 go 命令就会先读 go.mod 并
# 触发工具链切换，抛出的 `invalid GOTOOLCHAIN "go99.0"` 之类报错比不检查还糟。
MIN_GO=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' go.mod | head -n1)
CUR_GO=$(GOTOOLCHAIN=local go env GOVERSION)
CUR_GO=${CUR_GO#go}
if [ -n "$MIN_GO" ] &&
	[ "$(printf '%s\n%s\n' "$MIN_GO" "$CUR_GO" | sort -V | head -n1)" != "$MIN_GO" ]; then
	die "Go 版本过低：当前 $CUR_GO，项目要求 >= $MIN_GO。请先 pkg upgrade golang"
fi

# --- 拉最新代码 -------------------------------------------------------------
if [ "$DO_PULL" -eq 1 ]; then
	if git rev-parse --git-dir >/dev/null 2>&1; then
		if [ -n "$(git status --porcelain)" ]; then
			# 只是提醒，不是拦截：git 遇到会被覆盖的本地改动会自己中止。
			echo '! 工作区有未提交改动，git pull 冲突时会自动中止（不会丢你的修改）'
		fi
		ok '拉取最新代码…'
		# --ff-only：宁可失败也不自动生成合并提交 —— 手机上手工解冲突太痛苦。
		git pull --ff-only || die 'git pull 失败（可能有本地提交或冲突），请手动执行 git pull 查看原因'
		ok "当前提交 $(git rev-parse --short HEAD)"
	else
		echo '! 不是 git 仓库，跳过拉取（按当前代码编译）'
	fi
fi

# --- 停掉旧实例 ------------------------------------------------------------
# Linux 不允许覆盖正在运行的程序（ETXTBSY），所以必须先停再编。
# 模式要求命令后面跟空格或结束，避免 ./bin/codeforge-termux.sh 这类文件名被误杀。
if pkill -f '(^|/)bin/codeforge( |$)' 2>/dev/null; then
	ok '已停止旧实例'
fi

# --- 编译 ------------------------------------------------------------------
# CGO_ENABLED=0 / -trimpath / -ldflags "-s -w"：与 AGENTS.md、Makefile 一致。
# 纯静态单二进制，不依赖 Termux 的 NDK。
mkdir -p bin
ok '编译中（首次可能要几分钟）…'
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o "$BIN" ./cmd/agent ||
	die '编译失败'
ok "产物：$BIN（$(du -h "$BIN" | cut -f1)）"

# --- 跑测试（可选） --------------------------------------------------------
if [ "$DO_TEST" -eq 1 ]; then
	ok '跑 go test ./…（手机上比较慢）'
	go test ./... || die '测试未通过，已中止启动'
fi

if [ "$DO_RUN" -eq 0 ]; then
	ok "按 --no-run 跳过启动。手动启动：$BIN -config config"
	exit 0
fi

# 调用方自带 -config 时不追加（与 cf.cmd 同一处理），否则两份 -config 会打架。
CFG='-config config'
for a in ${APP_ARGS[@]+"${APP_ARGS[@]}"}; do
	case "$a" in
	-config | --config) CFG='' ;;
	esac
done

ok '启动（Ctrl-C 停止）'
# exec：让程序接管当前终端，信号直达 —— 否则 Ctrl-C 只会杀掉脚本，程序还活着占着端口。
exec "$BIN" $CFG ${APP_ARGS[@]+"${APP_ARGS[@]}"}
