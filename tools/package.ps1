#!/usr/bin/env pwsh
<#
.SYNOPSIS
    打包可发布的 QixinBot release（二进制 + SDK + 插件 + zip）。

.DESCRIPTION
    一条命令做完全部发布准备，并在最后**校验**：
      - 二进制是不是刚编的（晚于最新提交）
      - SDK 是不是和源码一致
      - 每个 .anzu 包能不能打开

    之前手工发布漏过 SDK 同步和二进制重建，所以把校验做进脚本里。

.EXAMPLE
    .\tools\package.ps1                    # 完整发布
    .\tools\package.ps1 -SkipZip           # 只更新 release 目录，不打包
#>
param(
    [switch]$SkipZip,
    [switch]$UseProxy,
    # 插件签名私钥。给了就用它签所有插件（统一密钥）。
    # 留空则不签名（开发模式）。
    [string]$Key = '',
    # 把插件一起打进 release/plugins（默认不打——插件有独立仓库）。
    [switch]$WithPlugins
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$release = Join-Path $root 'release'

function Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Fail($msg) { Write-Host "❌ $msg" -ForegroundColor Red; exit 1 }
function Ok($msg)   { Write-Host "  ✅ $msg" -ForegroundColor Green }

Set-Location $root

# ---------------------------------------------------------------- 0. 环境
Step '准备 Go 环境'
# go-env.ps1 在上上级目录（C:\Users\anzu\Documents\xin\tools）
$goEnv = Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) 'tools\go-env.ps1'
if (Test-Path $goEnv) { . $goEnv } else { Write-Host '  ⚠️ 没找到 go-env.ps1，用系统 Go' -ForegroundColor Yellow }
if ($UseProxy) {
    $env:HTTP_PROXY = 'http://127.0.0.1:7890'
    $env:HTTPS_PROXY = 'http://127.0.0.1:7890'
}

if (-not (Test-Path $release)) { New-Item -ItemType Directory -Path $release | Out-Null }
foreach ($d in 'bin', 'plugins', 'sdk\node', 'sdk\python') {
    $p = Join-Path $release $d
    if (-not (Test-Path $p)) { New-Item -ItemType Directory -Path $p -Force | Out-Null }
}

# ---------------------------------------------------------------- 1. 二进制
Step '编译二进制（Windows + Linux）'

$builds = @(
    @{ Out = 'qixinbot.exe';      Pkg = './cmd/bot';           OS = 'windows' },
    @{ Out = 'qixinpack.exe';     Pkg = './tools/qixinpack';   OS = 'windows' },
    @{ Out = 'mock-onebot.exe';   Pkg = './tools/mock-onebot'; OS = 'windows' },
    @{ Out = 'qixinbot';          Pkg = './cmd/bot';           OS = 'linux' },
    @{ Out = 'qixinpack';         Pkg = './tools/qixinpack';   OS = 'linux' }
)

foreach ($b in $builds) {
    $env:GOOS = $b.OS
    $env:GOARCH = 'amd64'
    $out = Join-Path $release "bin\$($b.Out)"
    go build -o $out $b.Pkg
    if ($LASTEXITCODE -ne 0) { Fail "编译 $($b.Out) 失败" }
    Ok "$($b.Out)  ($([math]::Round((Get-Item $out).Length/1MB,1)) MB)"
}
# 恢复本机目标
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'

# ---------------------------------------------------------------- 2. SDK
Step '同步 SDK'
Copy-Item 'sdk\node\qixin.mjs' 'release\sdk\node\qixin.mjs' -Force
Copy-Item 'sdk\python\qixin.py' 'release\sdk\python\qixin.py' -Force
Ok "qixin.mjs  $((Get-Content 'release\sdk\node\qixin.mjs').Count) 行"
Ok "qixin.py   $((Get-Content 'release\sdk\python\qixin.py').Count) 行"

# ---------------------------------------------------------------- 3. 插件
Step '打包插件（可选）'
$pkgs = @()
if (-not $WithPlugins) {
    Ok '跳过（插件走独立仓库 QixinBot-go-plugins；要打进发行包加 -WithPlugins）'
}
$built = 0
foreach ($p in $pkgs) {
    $out = Join-Path $release "plugins\$($p.Name).anzu"
    $packArgs = @('pack', "dist\$($p.Name)", '-out', $out)
    if ($Key) { $packArgs += @('-key', $Key) }
    & .\bin\qixinpack.exe @packArgs 2>&1 | Out-Null
    if ($LASTEXITCODE -ne 0) { Fail "打包 $($p.Name) 失败" }
    $built++
}
Ok "$built 个插件"

# ---------------------------------------------------------------- 4. 配置
Step '同步配置模板'
Copy-Item 'config.example.toml' 'release\config.toml' -Force
Ok 'config.toml'

# ---------------------------------------------------------------- 5. 校验
Step '校验'

# 5.1 SDK 必须和源码一致
$srcSdk = (Get-FileHash 'sdk\node\qixin.mjs').Hash
$relSdk = (Get-FileHash 'release\sdk\node\qixin.mjs').Hash
if ($srcSdk -ne $relSdk) { Fail 'release 的 qixin.mjs 和源码不一致' }
Ok 'SDK 与源码一致'

# 5.2 SDK 必须包含关键方法
$mjs = Get-Content 'release\sdk\node\qixin.mjs' -Raw
foreach ($m in 'ask:', 'background:', 'capability:', 'storage:') {
    if ($mjs -notmatch [regex]::Escape($m)) { Fail "SDK 缺方法 $m" }
}
Ok 'SDK 关键方法齐全（ask / background / capability / storage）'

# 5.3 二进制时间必须晚于最新提交
$lastCommit = (git log -1 --format=%ct) -as [long]
$lastCommitTime = [DateTimeOffset]::FromUnixTimeSeconds($lastCommit).LocalDateTime
foreach ($f in 'bin\qixinbot.exe', 'bin\qixinpack.exe') {
    $t = (Get-Item (Join-Path $release $f)).LastWriteTime
    if ($t -lt $lastCommitTime) {
        Fail "$f 比最新提交（$($lastCommitTime.ToString('HH:mm:ss'))）旧，可能是旧产物"
    }
}
Ok "二进制晚于最新提交（$($lastCommitTime.ToString('yyyy-MM-dd HH:mm:ss'))）"

# 5.4 每个 .anzu 都能打开
Add-Type -AssemblyName System.IO.Compression.FileSystem
foreach ($z in @(Get-ChildItem (Join-Path $release 'plugins') -Filter *.anzu -ErrorAction SilentlyContinue)) {
    try {
        $zip = [System.IO.Compression.ZipFile]::OpenRead($z.FullName)
        $hasManifest = ($zip.Entries | Where-Object { $_.FullName -eq 'manifest.toml' }).Count -gt 0
        $zip.Dispose()
        if (-not $hasManifest) { Fail "$($z.Name) 缺 manifest.toml" }
    } catch {
        Fail "$($z.Name) 打不开: $_"
    }
}
Ok "$($pkgs.Count) 个包结构正常"

# 5.5 **每个包都必须由这把钥匙签**（发布纪律的硬校验）
#
# 为什么逐个验：官方仓库里的包如果混进了别人签的，用户的框架会拒装，
# 而且管理员还得手动加公钥——等于把"官方"这个信任背书弄没了。
if ($Key) {
    $pub = [System.IO.Path]::ChangeExtension($Key, '.pub')
    if (-not (Test-Path $pub)) { Fail "找不到公钥 $pub" }

    # 从公钥算期望指纹（和 qixinpack check 显示的算法一致：SHA256 前 8 字节）
    $pubBytes = [Convert]::FromBase64String((Get-Content $pub -Raw).Trim())
    $sha = [System.Security.Cryptography.SHA256]::Create().ComputeHash($pubBytes)
    $expectedFp = (($sha[0..7] | ForEach-Object { $_.ToString('x2') }) -join '')

    $bad = @()
    foreach ($pkg in (Get-ChildItem (Join-Path $release 'plugins') -Filter *.anzu)) {
        $out = (& .\bin\qixinpack.exe check $pkg.FullName 2>&1) -join "`n"
        if ($out -notmatch '签发者: 指纹 ([0-9a-f]{16})') {
            $bad += "$($pkg.Name)（无签名者信息）"
            continue
        }
        if ($Matches[1] -ne $expectedFp) {
            $bad += "$($pkg.Name)（$($Matches[1]) ≠ $expectedFp）"
        }
    }
    if ($bad.Count -gt 0) {
        Fail "这些包不是官方密钥签的，用户装不上：`n  " + ($bad -join "`n  ")
    }
    Ok "全部 $((Get-ChildItem (Join-Path $release 'plugins') -Filter *.anzu).Count) 个包均由官方密钥签名（指纹 $expectedFp）"
}

# 5.6 插件清单里有 canvas（依赖它的插件能跑）——只在把插件打进包时才检查
if ($WithPlugins -and -not (Test-Path (Join-Path $release 'plugins\canvas.anzu'))) {
    Write-Host '  ⚠️  没有 canvas.anzu，cngal / fortune 会缺画布能力' -ForegroundColor Yellow
}

# ---------------------------------------------------------------- 6. zip
Step '打包 zip'
if (-not $SkipZip) {
    Get-ChildItem $root -Filter 'QixinBot-release-*.zip' | Remove-Item -Force
    $stamp = Get-Date -Format 'yyyyMMdd-HHmm'
    $zipPath = Join-Path $root "QixinBot-release-$stamp.zip"
    Compress-Archive -Path (Join-Path $release '*') -DestinationPath $zipPath -CompressionLevel Optimal
    Ok "$(Split-Path -Leaf $zipPath)  ($([math]::Round((Get-Item $zipPath).Length/1MB,1)) MB)"
}

# ---------------------------------------------------------------- 7. 汇报
Write-Host ''
Write-Host '===== 发布包就绪 =====' -ForegroundColor Green
Write-Host "目录: $release"
Write-Host ''
Get-ChildItem (Join-Path $release 'bin') | ForEach-Object {
    "  {0,-20} {1,6} MB" -f $_.Name, [math]::Round($_.Length/1MB, 1)
}
Write-Host ''
$pkgCount = (Get-ChildItem (Join-Path $release 'plugins') -Filter *.anzu -ErrorAction SilentlyContinue).Count
if ($pkgCount -gt 0) {
    Write-Host "插件: $pkgCount 个（已打进发行包）"
} else {
    Write-Host '插件: 不在发行包里（从 QixinBot-go-plugins 安装，或在控制台上传）'
}
Write-Host ''
Write-Host '推送到 Gitea:' -ForegroundColor Cyan
Write-Host '  cd release; git add -A; git commit -m "release: <说明>"; git push'
