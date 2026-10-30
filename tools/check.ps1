# 提交前检查：vet + 全量测试。
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    . (Join-Path (Split-Path -Parent $root) "tools\go-env.ps1")
    Write-Host "== go vet =="
    go vet ./...
    Write-Host "== go test =="
    go test ./... -count=1
    Write-Host "全部通过"
} finally {
    Pop-Location
}
