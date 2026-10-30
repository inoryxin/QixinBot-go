// 共享运行时宿主（multi-host）原型——cohost 分支实验件，未接入 Go 宿主。
//
// 职责：stdio 上的 JSON-RPC（帧带 plugin 字段）多路复用给每插件一个 worker；
// worker 发回的帧打上 plugin 标记后写 stdout。
//
// 用法：node multi-host.mjs <plugins.json>
//   { "sdkDir": "../sdk/node", "plugins": [{ "id", "dir", "entry"? }] }
import { Worker } from 'node:worker_threads'
import { join, resolve, dirname } from 'node:path'
import { pathToFileURL } from 'node:url'
import readline from 'node:readline'

const cfgPath = resolve(process.argv[2] || 'plugins.json')
const cfg = JSON.parse((await import('node:fs')).readFileSync(cfgPath, 'utf8'))
cfg.plugins = cfg.plugins || []
const cfgDir = dirname(cfgPath)
const sdkDir = resolve(cfgDir, cfg.sdkDir || '../sdk/node')
const entryScript = join(sdkDir, 'worker-entry.mjs')

const workers = new Map() // id -> Worker
const stopped = new Set() // 被 Go 宿主指令停止的插件（不自动重启）
const memStats = new Map() // id -> 最新内存画像（M0 用，不转发宿主）
const attempts = new Map() // id -> 连续重启次数（退避）
const lastExitAt = new Map() // id -> 上次退出时间（稳定窗口判定）
const respawnTimers = new Map() // id -> 重启定时器

const write = (frame) => {
  let line
  try {
    line = JSON.stringify(frame)
  } catch (e) {
    // 结构化克隆能传的东西（BigInt 等）JSON 序列化不了——
    // 丢帧并记日志，绝不能让 multi-host 整个崩掉（全组陪葬）
    werr(`帧序列化失败，已丢弃: ${e?.message || e}`)
    return
  }
  process.stdout.write(line + '\n')
}
const werr = (line) => process.stderr.write('[multi-host] ' + line + '\n')

function spawn(p) {
  // 防僵尸（stop 后退避定时器又拉起）+ 防双开（start 时旧退避定时器未清）
  if (stopped.has(p.id) || workers.has(p.id)) return
  const w = new Worker(entryScript, {
    env: {
      ...process.env,
      QIXIN_WORKER: '1',
      QIXIN_PLUGIN_ID: p.id,
      QIXIN_PLUGIN_DIR: resolve(cfgDir, p.dir),
      QIXIN_ENTRY: p.entry || 'app/index.js',
      QIXIN_DATA_DIR: p.data_dir || '',
    },
    // 插件（或其依赖）直写 stdout 时管道到这边收编成日志——
    // 默认不隔离的话会直接混进协议通道，一行日志污染全组
    stdout: true,
    stderr: true,
  })
  workers.set(p.id, w)
  w.stdout.on('data', (d) => werr(`[${p.id}] ` + d.toString().trimEnd()))
  w.stderr.on('data', (d) => werr(`[${p.id}] ` + d.toString().trimEnd()))

  w.on('message', (frame) => {
    if (!frame || typeof frame !== 'object') return
    if (frame.__mem) {
      // M0 内存自报：收集不转发
      memStats.set(p.id, { ...frame, at: Date.now() })
      return
    }
    if (frame.__worker_exit || frame.__ctl_result) return // 内部标不许 worker 伪造
    frame.plugin = p.id // 无条件打标：worker 不能伪造别的插件的来源
    write(frame)
  })
  w.on('error', (e) => werr(`worker ${p.id} 错误: ${e?.message || e}`)) // 不能静默：加载错误全靠它
  werr(`worker ${p.id} 已启动`)
  w.on('exit', (code) => {
    if (workers.get(p.id) !== w) return // 已被手动替换
    workers.delete(p.id)
    const stoppedNow = stopped.has(p.id)
    write({ __worker_exit: true, plugin: p.id, code, stopped: stoppedNow })
    if (stoppedNow) {
      werr(`worker ${p.id} 已按指令停止`)
      return
    }
    // 稳定窗口修正：距上次退出 >60s 视为新一轮（此前最早的稳定定时器
    // 会在循环崩溃中把 attempts 清零，永远退避不起来）
    const prev = lastExitAt.get(p.id) || 0
    const n = Date.now() - prev > 60000 ? 1 : (attempts.get(p.id) || 0) + 1
    lastExitAt.set(p.id, Date.now())
    attempts.set(p.id, n)
    const delay = Math.min(300 * 2 ** Math.min(n, 5), 10000)
    werr(`worker ${p.id} 退出（code=${code}，第 ${n} 次），${delay}ms 后重启`)
    const t = setTimeout(() => {
      respawnTimers.delete(p.id)
      spawn(p)
    }, delay)
    respawnTimers.set(p.id, t)
  })
}

for (const p of cfg.plugins) spawn(p)

// M0：默认关闭内存汇总；需要排查时设 QIXIN_DEBUG_MEM=1。
if (process.env.QIXIN_DEBUG_MEM === '1') setInterval(() => {
  const arr = [...memStats.entries()].map(([id, m]) => ({
    id,
    rssMB: +(m.rss / 1048576).toFixed(1),
    heapMB: +(m.heapUsed / 1048576).toFixed(1),
    extMB: +(m.external / 1048576).toFixed(1),
    abMB: +((m.arrayBuffers || 0) / 1048576).toFixed(1),
  }))
  if (arr.length === 0) return
  const heapMB = +arr.reduce((a, x) => a + x.heapMB, 0).toFixed(1)
  const extMB = +arr.reduce((a, x) => a + x.extMB, 0).toFixed(1)
  const top = [...arr].sort((a, b) => b.heapMB - a.heapMB).slice(0, 5)
  werr('[mem] ' + JSON.stringify({ count: arr.length, heapMB, extMB, top, all: arr }))
}, 30000)

// Go 宿主的控制帧（__ctl）：stop / reload / list。
// stop：terminate worker 且不自动重启（插件被禁用/删除时用）
// reload：terminate worker，自动重启会以新代码拉起（_sdk/文件已换）
// list：回一份 {__ctl_result, workers:[{id,alive}]}
function ctl(frame) {
  const op = frame.__ctl
  if (op === 'stop') {
    stopped.add(frame.plugin)
    const t = respawnTimers.get(frame.plugin)
    if (t) { clearTimeout(t); respawnTimers.delete(frame.plugin) }
    const w = workers.get(frame.plugin)
    if (w) w.terminate()
    werr(`ctl: stop ${frame.plugin}`)
  } else if (op === 'start') {
    // dir/entry 可随帧携带：Go 宿主运行时把新插件加进共享组
    let p = cfg.plugins.find((x) => x.id === frame.plugin)
    if (!p && frame.dir) {
      p = { id: frame.plugin, dir: frame.dir, entry: frame.entry || 'app/index.js', data_dir: frame.data_dir || '' }
      cfg.plugins.push(p)
    }
    stopped.delete(frame.plugin)
    const t = respawnTimers.get(frame.plugin)
    if (t) { clearTimeout(t); respawnTimers.delete(frame.plugin) }
    if (p && !workers.has(frame.plugin)) {
      spawn(p)
      werr(`ctl: start ${frame.plugin}`)
    }
  } else if (op === 'reload') {
    const w = workers.get(frame.plugin)
    if (w) w.terminate() // 自动重启会重新 import（拿到新代码）
    werr(`ctl: reload ${frame.plugin}`)
  } else if (op === 'list') {
    write({ __ctl_result: true, op: 'list', workers: cfg.plugins.map((p) => ({ id: p.id, alive: workers.has(p.id), stopped: stopped.has(p.id) })) })
  }
}

// 宿主 → worker：按 plugin 字段路由
const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity })
rl.on('line', (line) => {
  const text = line.trim()
  if (!text) return
  let frame
  try {
    frame = JSON.parse(text)
  } catch {
    werr('无法解析的输入行（丢弃）：' + text.slice(0, 120))
    return
  }
  if (frame.__ctl) {
    ctl(frame)
    return
  }
  const id = frame.plugin
  const w = id ? workers.get(id) : undefined
  if (!w) {
    if (frame.id) {
      write({
        jsonrpc: '2.0',
        id: frame.id,
        plugin: id || '',
        error: { code: -32002, message: `插件 ${id || '(未指定)'} 不在共享宿主中` },
      })
    }
    werr(`帧路由失败：plugin=${id || '(未指定)'}`)
    return
  }
  w.postMessage(frame)
})
rl.on('close', () => {
  // 宿主断了：全组退出
  for (const [, w] of workers) w.terminate()
  process.exit(0)
})
werr(`共享宿主就绪：${cfg.plugins.map((p) => p.id).join(', ')}`)
