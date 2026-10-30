# 构建发行版。
# 用法：
#   .\tools\build.ps1              # 发行版 -> bin/qixinbot.exe
param()

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    . (Join-Path (Split-Path -Parent $root) "tools\go-env.ps1")
    $out = "bin/qixinbot.exe"
    go build -o $out ./cmd/bot
    Write-Host "构建完成: $out"
} finally {
    Pop-Location
}
