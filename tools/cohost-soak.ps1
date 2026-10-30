# cohost 稳定性浸泡采集：每 5 分钟写一行 JSONL，第二天直接看趋势。
# 部署位置：C:\qqbot\data\cohost-soak\collect.ps1
$ErrorActionPreference = 'Stop'
$outDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$now = Get-Date
$day = $now.ToString('yyyyMMdd')
# 每次框架重启换 tag，避免把重启前后的数据混成一段
$runTag = '85cab79'
$jsonl = Join-Path $outDir "soak-$day-$runTag.jsonl"
$latest = Join-Path $outDir 'latest.json'
$base = 'http://127.0.0.1:7801'

$record = [ordered]@{
  ts = $now.ToString('o')
  ok = $false
}

try {
  $s = New-Object Microsoft.PowerShell.Commands.WebRequestSession
  Invoke-WebRequest -Uri "$base/login" -Method Post -Body @{ username = 'admin'; password = 'qixin2026' } -WebSession $s -UseBasicParsing -TimeoutSec 10 | Out-Null

  $ov = Invoke-RestMethod -Uri "$base/api/overview" -WebSession $s -TimeoutSec 15
  $metrics = Invoke-RestMethod -Uri "$base/api/metrics" -WebSession $s -TimeoutSec 15

  $procs = @(Get-Process qixinbot, node -ErrorAction SilentlyContinue | Select-Object Id, ProcessName, WorkingSet64, PrivateMemorySize64, StartTime, Path)
  $pluginStates = [ordered]@{}
  foreach ($p in @($ov.plugins)) {
    $pluginStates[$p.id] = [ordered]@{
      state = $p.state
      pid = $p.pid
      rss_mb = $p.memory_mb
    }
  }

  $record.ok = $true
  $record.connected = [bool]$ov.accounts[0].connected
  $record.self_id = $ov.accounts[0].self_id
  $record.uptime_seconds = $ov.uptime_seconds
  $record.plugins_total = @($ov.plugins).Count
  $record.plugins_nonready = @($ov.plugins | Where-Object { $_.state -notin @('ready', 'idle') }).Count
  $record.plugin_states = $pluginStates
  $record.processes = @($procs | ForEach-Object {
      [ordered]@{
        pid = $_.Id
        name = $_.ProcessName
        rss_mb = [math]::Round($_.WorkingSet64 / 1MB, 1)
        private_mb = [math]::Round($_.PrivateMemorySize64 / 1MB, 1)
        start = $_.StartTime.ToString('o')
        path = $_.Path
      }
    })
  $record.process_count = $procs.Count
  $record.process_rss_mb = [math]::Round((($procs | Measure-Object WorkingSet64 -Sum).Sum) / 1MB, 1)
  $record.metrics_windows = @($metrics.windows).Count

  # 最近错误日志，给第二天复查留上下文
  $logPath = 'C:\qqbot\data\bot.log'
  if (Test-Path $logPath) {
    $record.recent_errors = @(Select-String -Path $logPath -Pattern 'level=ERROR|level=WARN' -Encoding utf8 | Select-Object -Last 8 | ForEach-Object { $_.Line })
  }
} catch {
  $record.error = $_.Exception.Message
}

$json = $record | ConvertTo-Json -Depth 8 -Compress
Add-Content -Path $jsonl -Value $json -Encoding utf8
Set-Content -Path $latest -Value ($record | ConvertTo-Json -Depth 8) -Encoding utf8
