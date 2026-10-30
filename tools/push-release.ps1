# 把当前二进制推到 release 仓库；二进制没变就什么都不做。
# 用法: .\tools\push-release.ps1 -Message "说明"
param([string]$Message = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
$rel = Join-Path $root "release"
$bin = Join-Path $root "bin\qixinbot.exe"
$dst = Join-Path $rel "bin\qixinbot.exe"

if (-not (Test-Path $bin)) { throw "没找到 $bin，先构建" }

$newHash = (Get-FileHash $bin -Algorithm SHA256).Hash
$oldHash = ""
if (Test-Path $dst) { $oldHash = (Get-FileHash $dst -Algorithm SHA256).Hash }

if ($newHash -eq $oldHash) {
    Write-Host "二进制没变，跳过 release 推送" -ForegroundColor Yellow
    exit 0
}

Copy-Item $bin $dst -Force
Push-Location $root
$commit = (git rev-parse --short HEAD)
$time = (Get-Date -Format 'yyyy-MM-dd HH:mm')
@"
# 这个发行包对应哪份源码

| | |
|---|---|
| 源码仓库 | ``QixinBot-go`` |
| 提交 | ``$commit`` |
| 构建时间 | ``$time`` |

发行包只放**框架本体**（二进制 + SDK + 配置模板），**源码不在这个仓库**：
源码看 ``QixinBot-go``，插件看 ``QixinBot-go-plugins``。
"@ | Set-Content (Join-Path $rel "SOURCE.md") -Encoding UTF8
Pop-Location

Push-Location $rel
git add -A
$msg = if ($Message) { "$Message (source $commit)" } else { "release: build from $commit" }
git -c user.name="anzu" -c user.email="anzu@localhost" commit -q -m $msg
git push
Pop-Location
Write-Host "release 已更新：$msg" -ForegroundColor Green
