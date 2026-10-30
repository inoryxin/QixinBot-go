'use strict'

// 一台 PC 只跑这一个程序：状态、截图、文件、操作、DSH 唤醒与连接。
// 由 Windows 计划任务在登录时启动，监听 3097，供 QQ 插件调用。

const http = require('node:http')
const fs = require('node:fs')
const os = require('node:os')
const path = require('node:path')
const net = require('node:net')
const { spawn, execFile } = require('node:child_process')

const PORT = Number(process.env.PC_AGENT_PORT || 3097)
// 这个进程有 /shell 和 /file：没有 token 就拒绝启动，绝不能有源码默认值
const TOKEN = String(process.env.PC_AGENT_TOKEN || '')
if (!TOKEN) {
  console.error('缺少访问 token：设置环境变量 PC_AGENT_TOKEN 后再启动（首次安装跑 install.ps1 会自动生成并存到 token.txt）')
  process.exit(2)
}
const HOME = os.homedir()
const DSH_HOME = path.join(HOME, '.dsh')
const DSH_REPO = process.env.DSH_REPO || 'D:\\deepseek\\deepseek-harness'
const LOG_DIR = path.join(DSH_HOME, 'logs')
const AGENT_LOG = path.join(LOG_DIR, 'pc-agent.log')
const DSH_WEB_OUT = path.join(LOG_DIR, 'dsh-web.out.log')
const DSH_WEB_ERR = path.join(LOG_DIR, 'dsh-web.err.log')
const DSH_WEB_URL = path.join(DSH_HOME, 'web-url.txt')
const ACP_BRIDGE = path.join(DSH_HOME, 'acp-bridge.js')
const SCREEN_DIR = path.join(LOG_DIR, 'screens')
const UPLOAD_DIR = path.join(DSH_HOME, 'qq-uploads')

fs.mkdirSync(LOG_DIR, { recursive: true })
fs.mkdirSync(SCREEN_DIR, { recursive: true })
fs.mkdirSync(UPLOAD_DIR, { recursive: true })

function log(message) {
  try { fs.appendFileSync(AGENT_LOG, `${new Date().toISOString()} ${message}\n`) } catch {}
}

function portOpen(port, host = '127.0.0.1', timeoutMs = 800) {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port })
    const done = (value) => { socket.destroy(); resolve(value) }
    socket.setTimeout(timeoutMs, () => done(false))
    socket.once('connect', () => done(true))
    socket.once('error', () => done(false))
  })
}

function run(file, args, options = {}) {
  return new Promise((resolve, reject) => {
    execFile(file, args, { windowsHide: true, maxBuffer: 8 << 20, timeout: 30000, ...options }, (error, stdout, stderr) => {
      if (error) reject(new Error(String(stderr || error.message).trim()))
      else resolve(String(stdout))
    })
  })
}

async function powershell(script, timeout = 30000) {
  const encoded = Buffer.from(script, 'utf16le').toString('base64')
  return run('powershell.exe', ['-NoLogo', '-NoProfile', '-NonInteractive', '-EncodedCommand', encoded], { timeout })
}

async function status() {
  let disks = []
  let battery = null
  try {
    const raw = await powershell(`
$ErrorActionPreference='SilentlyContinue'
$d = Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | ForEach-Object { [pscustomobject]@{ name=$_.DeviceID; total=[double]$_.Size; free=[double]$_.FreeSpace } }
$b = Get-CimInstance Win32_Battery | Select-Object -First 1
[pscustomobject]@{ disks=@($d); battery=if($b){[double]$b.EstimatedChargeRemaining}else{$null} } | ConvertTo-Json -Depth 4 -Compress
`, 15000)
    const data = JSON.parse(raw)
    disks = (data.disks || []).filter((d) => d && d.total)
    battery = data.battery == null ? null : { percent: Number(data.battery) }
  } catch (error) {
    log(`status powershell failed: ${error.message}`)
  }
  const cpus = os.cpus()
  const total = os.totalmem()
  const free = os.freemem()
  return {
    host: os.hostname(),
    platform: `${os.platform()} ${os.release()} (${os.arch()})`,
    uptimeSec: Math.round(os.uptime()),
    cpu: {
      model: cpus[0]?.model || 'CPU',
      cores: cpus.length,
      load: os.loadavg().map((n) => Math.round(n * 100) / 100),
    },
    memory: { total, free },
    disks,
    battery,
    dsh: {
      web: await portOpen(3080),
      bridge: await portOpen(3099),
      agent: true,
    },
    ts: Date.now(),
  }
}

async function screenshot() {
  const file = path.join(SCREEN_DIR, `screen-${Date.now()}.jpg`)
  await powershell(`
Add-Type -AssemblyName System.Windows.Forms,System.Drawing
$b = [System.Windows.Forms.SystemInformation]::VirtualScreen
$bmp = New-Object System.Drawing.Bitmap $b.Width, $b.Height
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($b.Left, $b.Top, 0, 0, $bmp.Size)
$bmp.Save('${file.replace(/'/g, "''")}', [System.Drawing.Imaging.ImageFormat]::Jpeg)
$g.Dispose(); $bmp.Dispose()
`, 20000)
  return file
}

function launch(name, file, args, options = {}) {
  try {
    const out = fs.openSync(path.join(LOG_DIR, `${name}.out.log`), 'a')
    const err = fs.openSync(path.join(LOG_DIR, `${name}.err.log`), 'a')
    const child = spawn(file, args, {
      cwd: options.cwd || HOME,
      detached: true,
      stdio: ['ignore', out, err],
      windowsHide: true,
      env: { ...process.env, ...(options.env || {}) },
    })
    child.on('error', (error) => log(`${name} spawn error: ${error.message}`))
    child.unref()
    fs.closeSync(out); fs.closeSync(err)
    log(`${name} started pid=${child.pid}`)
    return child.pid
  } catch (error) {
    log(`${name} launch failed: ${error.message}`)
    return 0
  }
}

function readWebUrl() {
  try {
    const text = fs.readFileSync(DSH_WEB_OUT, 'utf8')
    const matches = [...text.matchAll(/https?:\/\/\S+\?token=[A-Za-z0-9_-]+/g)]
    if (matches.length) return matches[matches.length - 1][0]
  } catch {}
  try { return fs.readFileSync(DSH_WEB_URL, 'utf8').trim() } catch { return '' }
}

async function waitPort(port, timeoutMs) {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    if (await portOpen(port)) return true
    await new Promise((resolve) => setTimeout(resolve, 800))
  }
  return false
}

async function ensureDsh(withWeb = false) {
  const result = { bridge: { ready: false, started: false }, web: { ready: false, started: false, url: '' } }
  if (!(await portOpen(3099))) {
    if (fs.existsSync(ACP_BRIDGE)) {
      launch('acp-bridge', process.execPath, [ACP_BRIDGE], { cwd: DSH_HOME })
      result.bridge.started = true
    } else {
      result.bridge.error = `缺少 ${ACP_BRIDGE}`
    }
  }
  result.bridge.ready = await waitPort(3099, 15000)

  if (withWeb) {
    if (!(await portOpen(3080))) {
      launch('dsh-web', process.execPath, ['--import', 'tsx/esm', 'apps/cli/src/bin.ts', 'web', '--no-open'], { cwd: DSH_REPO })
      result.web.started = true
    }
    result.web.ready = await waitPort(3080, 45000)
    result.web.url = readWebUrl()
  } else {
    result.web.ready = await portOpen(3080)
    result.web.url = result.web.ready ? readWebUrl() : ''
  }
  return result
}

function action(name) {
  const cmd = {
    shutdown: ['shutdown.exe', ['/s', '/t', '0']],
    restart: ['shutdown.exe', ['/r', '/t', '0']],
    sleep: ['rundll32.exe', ['powrprof.dll,SetSuspendState', '0,1,0']],
    hibernate: ['shutdown.exe', ['/h']],
    lock: ['rundll32.exe', ['user32.dll,LockWorkStation']],
    logoff: ['shutdown.exe', ['/l']],
  }[String(name || '').toLowerCase()]
  if (!cmd) throw new Error(`未知操作: ${name}`)
  launch('action', cmd[0], cmd[1], {})
  return { action: name, ok: true }
}

function send(res, code, body, type = 'application/json; charset=utf-8') {
  const data = Buffer.isBuffer(body) ? body : Buffer.from(typeof body === 'string' ? body : JSON.stringify(body))
  res.writeHead(code, { 'Content-Type': type, 'Content-Length': data.length })
  res.end(data)
}

async function readBody(req, limit = 32 << 20) {
  const chunks = []
  let size = 0
  for await (const chunk of req) {
    size += chunk.length
    if (size > limit) throw new Error('请求体过大')
    chunks.push(chunk)
  }
  return Buffer.concat(chunks)
}

// timingSafeEqual：别让逐字节比较的时序把 token 磨出来
function authorized(req) {
  const value = String(req.headers.authorization || '')
  const want = `Bearer ${TOKEN}`
  const a = Buffer.from(value)
  const b = Buffer.from(want)
  if (a.length !== b.length) return false
  return require('node:crypto').timingSafeEqual(a, b)
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://${req.headers.host || '127.0.0.1'}`)
  try {
    if (url.pathname === '/healthz') return send(res, 200, { ok: true, host: os.hostname() })
    if (!authorized(req)) return send(res, 401, { error: 'unauthorized' })

    if (req.method === 'GET' && url.pathname === '/status') return send(res, 200, await status())
    if (req.method === 'GET' && url.pathname === '/dsh-url') return send(res, 200, { url: readWebUrl() })
    if (req.method === 'POST' && url.pathname === '/ensure-dsh') {
      const data = await ensureDsh(url.searchParams.get('web') === '1')
      return send(res, 200, data)
    }
    if (req.method === 'GET' && url.pathname === '/screen') {
      const file = await screenshot()
      const data = fs.readFileSync(file)
      return send(res, 200, data, 'image/jpeg')
    }
    if (req.method === 'POST' && url.pathname === '/open') {
      const body = await readBody(req)
      let target = url.searchParams.get('url') || ''
      if (!target && body.length) {
        try { target = JSON.parse(body.toString('utf8')).url || '' } catch {}
      }
      if (!/^https?:\/\//i.test(target)) return send(res, 400, { error: '需要 http(s) URL' })
      launch('open', 'cmd.exe', ['/c', 'start', '', target], {})
      return send(res, 200, { ok: true, url: target })
    }
    if (req.method === 'POST' && url.pathname === '/action') {
      const body = await readBody(req)
      const data = body.length ? JSON.parse(body.toString('utf8')) : {}
      return send(res, 200, action(url.searchParams.get('action') || data.action))
    }
    if (req.method === 'POST' && url.pathname === '/shell') {
      const body = await readBody(req)
      const data = body.length ? JSON.parse(body.toString('utf8')) : {}
      const command = String(url.searchParams.get('command') || data.command || '')
      if (!command) return send(res, 400, { error: '需要 command' })
      const output = await powershell(command, Number(data.timeout) || 60000)
      return send(res, 200, { ok: true, output })
    }
    if (req.method === 'POST' && url.pathname === '/upload') {
      const name = path.basename(url.searchParams.get('name') || `upload-${Date.now()}.bin`)
      const body = await readBody(req)
      const file = path.join(UPLOAD_DIR, name)
      fs.writeFileSync(file, body)
      return send(res, 200, { ok: true, path: file, bytes: body.length })
    }
    if (req.method === 'GET' && url.pathname === '/file') {
      const file = String(url.searchParams.get('path') || '')
      if (!file || !fs.existsSync(file) || !fs.statSync(file).isFile()) return send(res, 404, { error: '文件不存在' })
      const data = fs.readFileSync(file)
      const ext = path.extname(file).toLowerCase()
      const type = ext === '.png' ? 'image/png' : ext === '.gif' ? 'image/gif' : ext === '.webp' ? 'image/webp' : 'image/jpeg'
      return send(res, 200, data, type)
    }
    return send(res, 404, { error: 'not found' })
  } catch (error) {
    log(`request ${req.method} ${req.url} failed: ${error.stack || error.message}`)
    send(res, 500, { error: error.message })
  }
})

server.listen(PORT, '0.0.0.0', () => log(`pc-agent listening 0.0.0.0:${PORT}`))
