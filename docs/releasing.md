# GitHub Release 发布流程

## 确认版本与源码

1. 检查 [GitHub Releases](https://github.com/raine-x/CodeForge-GO/releases)、`git tag --list` 和 `git ls-remote --tags github`。确认版本号可用，并核对 GitHub 账号对 `raine-x/CodeForge-GO` 有写入权限。
2. 确认要发布的提交。以干净检出的提交为构建来源，使 GitHub 标签、源码归档和二进制对应同一个版本。确认后将提交推送到 GitHub。
3. 在仓库根目录用 Go 1.25 或更新版本执行 `go vet ./...`、`go test ./...` 和 `make test-web`。没有 Make 时直接用 Node 运行 `web/test/` 下的三个 `*.test.js` 文件。

## 生成附件

Bash 下执行 `make all-platforms`。Windows PowerShell 在仓库根目录执行以下等效命令。

```powershell
$env:CGO_ENABLED = '0'
$targets = @(
  @{ OS = 'windows'; Arch = 'amd64'; File = 'codeforge-windows-amd64.exe' }
  @{ OS = 'linux';   Arch = 'amd64'; File = 'codeforge-linux-amd64' }
  @{ OS = 'android'; Arch = 'arm64'; File = 'codeforge-android-arm64' }
)
foreach ($target in $targets) {
  $env:GOOS = $target.OS
  $env:GOARCH = $target.Arch
  go build -trimpath -ldflags '-s -w' -o "bin/$($target.File)" ./cmd/agent
  if ($LASTEXITCODE -ne 0) { throw "Build failed for $($target.File)" }
}
$targets | ForEach-Object {
  $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath "bin/$($_.File)").Hash.ToLowerInvariant()
  "$hash  $($_.File)"
} | Set-Content -Encoding ascii bin/SHA256SUMS.txt
Remove-Item Env:GOOS, Env:GOARCH
```

| 平台 | 上传附件 |
|---|---|
| Windows x64 | `codeforge-windows-amd64.exe` |
| Linux x64 | `codeforge-linux-amd64` |
| Android ARM64（Termux） | `codeforge-android-arm64` |
| SHA-256 校验值 | `SHA256SUMS.txt` |

当前 Makefile 提供这三个构建目标。Windows MSI 需要独立的安装包定义与安装、卸载验证。

## 保存 Release 草稿

1. 在 [GitHub Releases](https://github.com/raine-x/CodeForge-GO/releases) 创建草稿。新标签应指向步骤一确认的提交。
2. 加入发布说明和上表附件。GitHub 会按标签提供源码压缩包；用户可从同版本源码取用 `config/` 和 `LICENSE`。
3. 下载草稿附件复核 SHA-256。在各目标平台启动程序并打开浏览器界面。Termux 下把二进制放在 `$HOME` 并赋执行权限。核对版本、目标提交和附件后再发布。候选版勾选 **Set as a pre-release**。
