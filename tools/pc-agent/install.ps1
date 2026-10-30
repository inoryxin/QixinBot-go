# 安装/更新本机 PC 管家（Go 单文件，不需要管理员）
$ErrorActionPreference = "Stop"
$repo = "C:\Users\anzu\Documents\xin\onebot-framework"
$binDir = Join-Path $env:USERPROFILE ".dsh\bin"
$exe = Join-Path $binDir "pc-agent.exe"
$tokenFile = Join-Path $binDir "token.txt"
$startup = Join-Path $env:APPDATA "Microsoft\Windows\Start Menu\Programs\Startup"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null

# 编到工作区外的 bin 目录，避免工作区沙箱把 exe 压成 Low IL
Get-Process pc-agent -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1
Push-Location $repo
try {
  go build -ldflags="-H=windowsgui" -o $exe ".\tools\pc-agent"
  if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
} finally { Pop-Location }

if (Test-Path $tokenFile) {
  $token = (Get-Content $tokenFile -Raw).Trim()
} else {
  $bytes = New-Object byte[] 24
  [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  $token = [Convert]::ToBase64String($bytes)
  Set-Content -Path $tokenFile -Value $token -Encoding ASCII
}
if (-not $token) { throw "token.txt 存在但内容为空，请删除后重装" }

# 登录快捷方式：直接启动 bin 里的 exe
$ws = New-Object -ComObject WScript.Shell
$lnk = $ws.CreateShortcut((Join-Path $startup "PCAgent.lnk"))
$lnk.TargetPath = $exe
$lnk.Arguments = "-port 3097 -token `"$token`""
$lnk.WorkingDirectory = $binDir
$lnk.Save()

# 清掉旧的启动脚本，避免和快捷方式重复
foreach ($name in "pc-agent.cmd", "pc-agent.vbs") {
  $p = Join-Path $startup $name
  if (Test-Path $p) { Remove-Item $p -Force }
}

if (-not (Get-Process pc-agent -ErrorAction SilentlyContinue)) {
  Start-Process -FilePath $exe -ArgumentList '-port','3097','-token',$token -WindowStyle Hidden
}
Write-Output "PC 管家已安装：$exe"
Write-Output "token 文件：$tokenFile"
Write-Output "登录快捷方式：$(Join-Path $startup 'PCAgent.lnk')"
