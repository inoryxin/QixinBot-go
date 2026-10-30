// QixinBot Node.js 插件适配器
//
// 把任意 JS 插件接到宿主上：宿主用 stdio 上的 JSON-RPC 2.0 和它说话。
// 用法见 plugins/echo/index.js。
//
// 两条不可协商的规则：
//   1. stdout 只走协议，任何日志都会被重定向到 stderr；
//   2. 一行一个 JSON（NDJSON）。

import readline from 'node:readline'
import { parentPort } from 'node:worker_threads'

// 共享宿主模式（QIXIN_WORKER=1）：跑在 worker 线程里，
// 协议帧走 parentPort（multi-host 打上 plugin 标记后转发给宿主），
// 不碰 process.stdout——worker 的 stdout 会被管道到主进程，污染协议通道
const WORKER = process.env.QIXIN_WORKER === '1'
const PLUGIN_ID = process.env.QIXIN_PLUGIN_ID || ''

const writeStderr = (...args) => {
  const line = args
    .map((a) => (typeof a === 'string' ? a : safeJson(a)))
    .join(' ')
  if (WORKER && parentPort) {
    // worker 里统一走结构化日志（multi-host 转发 stderr 并打上插件标）
    try {
      parentPort.postMessage({
        jsonrpc: '2.0',
        method: 'host.log',
        params: { level: 'info', message: line },
      })
      return
    } catch {}
  }
  process.stderr.write(line + '\n')
}

const safeJson = (v) => {
  try {
    return JSON.stringify(v)
  } catch {
    return String(v)
  }
}

// console.log 默认写 stdout，会污染协议通道——这是最常见的翻车点。
console.log = writeStderr
console.info = writeStderr
console.warn = writeStderr
console.debug = writeStderr
console.error = writeStderr

let seq = 0
let jobSeq = 0
const pending = new Map()

function send(frame) {
  if (WORKER) {
    if (!parentPort) throw new Error('worker 模式但没有 parentPort')
    frame.plugin = PLUGIN_ID // 共享宿主多路复用：宿主按 (plugin, id) 路由
    parentPort.postMessage(frame)
    return
  }
  process.stdout.write(JSON.stringify(frame) + '\n')
}

function request(method, params) {
  const id = `p-${++seq}`
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject })
    send({ jsonrpc: '2.0', id, method, params })
  })
}

/** 宿主侧能力。插件只能通过这些方法影响外部世界。 */
export const host = {
  reply: (chat, segments, replyTo) =>
    request('host.reply', { chat, segments, reply_to: replyTo ?? 0 }),
  sendMessage: (chat, segments) => request('host.send_message', { chat, segments }),

  /**
   * 起一个后台任务，**不阻塞** handle_message。
   *
   *   async handle_message({ session }) {
   *     host.background(session, '查订阅', async () => {
   *       const items = await slowFetch()          // 可能 30 秒
   *       await host.reply(chatOf(session), 结果)
   *     })
   *     return { handled: true }                   // 立刻返回
   *   }
   *
   * 任务会出现在控制台「工具」页的后台任务表里。
   * **注意**：插件进程被杀时，后台任务也一起没了——要跨重启就把进度/上次时间存 host.storage，启动时续跑。
   */
  background: (session, name, fn) => {
    const id = 'job-' + (++jobSeq) + '-' + Date.now().toString(36)
    const chat = session ? chatOf(session) : null
    // 只记账，不等它
    request('host.job_begin', { id, name, chat }).catch(() => {})
    // then(onOk, onErr) 保证 job_end 恰好发一次：
    // 之前 catch 发一次 error 版、then 又发一次正常版，
    // 宿主会把失败标记覆盖成"成功结束"
    Promise.resolve()
      .then(fn)
      .then(
        () => request('host.job_end', { id }).catch(() => {}),
        (e) => {
          request('host.log', { level: 'error', message: `后台任务「${name}」失败: ${e?.message || e}` }).catch(() => {})
          return request('host.job_end', { id, error: String(e?.message || e) }).catch(() => {})
        },
      )
    return id
  },

  /**
   * 发一句提示，等用户下一条消息。
   *
   *   const city = await host.ask(chat, '请输入城市名（60 秒内回复）')
   *   if (!city) return { handled: true }
   *
   * 超时返回空字符串，不抛异常。
   */
  ask: async (chat, text, opts = {}) => {
    const r = await request('host.ask', {
      chat,
      text,
      user_id: (chat && chat.user_id) || opts.user_id || 0,
      timeout_sec: opts.timeout_sec || 60,
    })
    return r?.text || ''
  },
  callOnebot: (action, params) => request('host.call_onebot', { action, params }),
  // 宿主的 call_capability 返回 {result: ...}，这里剥掉一层，插件直接用 {ok, data, error}
  capability: (name, args) => request('host.call_capability', { name, args }).then((r) => r?.result ?? null),
  storage: {
    get: (key) => request('host.storage_get', { key }).then((r) => r?.value ?? null),
    set: (key, value) => request('host.storage_set', { key, value }),
    delete: (key) => request('host.storage_delete', { key }),
    list: (prefix = '') => request('host.storage_list', { prefix }).then((r) => r?.keys ?? []),
  },
  log: (level, message, fields) =>
    send({ jsonrpc: '2.0', method: 'host.log', params: { level, message, fields } }),
  /** 问宿主：这个会话能不能用某个插件 */
  checkPermission: (pluginId, session) =>
    request('host.check_permission', { plugin: pluginId, session }).then((r) => r?.allowed === true),
  /** 读一条共享数据（完整 key，如 ai.persona） */
  sharedGet: (key) => request('host.shared_get', { key }).then((r) => (r?.found ? r.value : null)),
  /** 写自己在 [[shared]] 里声明过的 key */
  sharedSet: (key, value) => request('host.shared_set', { key, value }),
  /** 列出全部共享数据 */
  sharedList: () => request('host.shared_list', {}).then((r) => r?.items ?? []),
  /** 执行一个 [[tools]] 工具（注意：不是能力，工具走另一条通道） */
  callTool: (name, args, session) =>
    request('host.call_tool', { name, args, session }).then((r) => r?.result ?? null),
  /** 拿这个会话能用的工具列表（传 null 就是全部） */
  listTools: (session) => request('host.list_tools', { session }).then((r) => r?.tools ?? []),
  /** 拿所有插件的权限配置 */
  listAccess: () => request('host.list_access', {}).then((r) => r?.access ?? {}),
}

/** 从 session 里取出聊天目标，省得每个插件手写。
 *  带 user_id：host.ask 靠 (chat_type, chat_id, user_id) 登记等待者，
 *  不带的话等待者永远注册在 user_id=0 上，用户的回复永远对不上号。 */
export function chatOf(session) {
  return {
    account: session.account,
    chat_type: session.chat_type,
    chat_id: session.chat_id,
    user_id: session.user_id,
  }
}

/** 没实现的方法用这些默认值，最小插件只需要写 handle_message。
 *  observe_message / http 也在默认里：声明了 observe=true 却没实现
 *  observe_message 时，之前会每次调用都吃 -32601（observe 还是静默失败），很难排查。 */
const DEFAULTS = {
  initialize: () => ({ api_version: '1.0' }),
  ping: () => ({ pong: true }),
  handle_message: () => ({ handled: false }),
  handle_event: () => ({}),
  observe_message: () => ({}),
  shutdown: () => ({}),
  list_tools: () => ({ tools: [] }),
  get_settings: () => ({ values: {} }),
  set_settings: () => ({ ok: true }),
  call_tool: () => ({ ok: false, error: '该插件没有实现工具' }),
  http: () => ({ status: 404, content_type: 'text/plain; charset=utf-8', body: Buffer.from('no http handler').toString('base64') }),
}

/** 处理一帧宿主消息（请求/响应/通知）。stdio 和 worker 两种模式共用。 */
async function handleFrame(text, impl) {
  let msg
  try {
    msg = JSON.parse(text)
  } catch {
    writeStderr(`适配器：无法解析的输入行：${text.slice(0, 120)}`)
    return
  }

  // 宿主对我方请求的响应
  if (msg.id && pending.has(msg.id)) {
    const { resolve, reject } = pending.get(msg.id)
    pending.delete(msg.id)
    if (msg.error) {
      const err = new Error(msg.error.message)
      err.code = msg.error.code
      reject(err)
    } else {
      resolve(msg.result)
    }
    return
  }

  if (!msg.method) return

  const name = msg.method.startsWith('plugin.') ? msg.method.slice(7) : msg.method
  const fn = impl[name] ?? DEFAULTS[name]

  // notification（无 id）不需要回包
  if (msg.id === undefined) {
    try {
      if (fn) await fn(msg.params ?? {})
    } catch (e) {
      writeStderr(`适配器：处理 ${msg.method} 失败：${e.message}`)
    }
    return
  }

  if (typeof fn !== 'function') {
    send({
      jsonrpc: '2.0',
      id: msg.id,
      error: { code: -32601, message: `方法未实现：${msg.method}` },
    })
    return
  }

  try {
    const result = await fn(msg.params ?? {})
    send({ jsonrpc: '2.0', id: msg.id, result: result ?? {} })
  } catch (e) {
    send({
      jsonrpc: '2.0',
      id: msg.id,
      error: { code: -32603, message: e?.message ?? String(e) },
    })
  }
}

/** 启动插件主循环。impl 里的方法名对应 plugin.* 方法。 */
export function serve(impl) {
  if (WORKER) return serveWorker(impl)

  const rl = readline.createInterface({ input: process.stdin, crlfDelay: Infinity })

  rl.on('line', async (line) => {
    await handleFrame(line.trim(), impl)
  })

  rl.on('close', () => {
    // 宿主断了：所有等回包的调用立刻失败，不然插件会永远挂起
    for (const { reject } of pending.values()) {
      reject(new Error('宿主已断开'))
    }
    pending.clear()
    process.exit(0)
  })
}

/** worker 模式的主循环：帧从 parentPort 进来（multi-host 路由）。 */
function serveWorker(impl) {
  if (!parentPort) throw new Error('QIXIN_WORKER=1 但不在 worker 线程里')
  parentPort.on('message', async (frame) => {
    // multi-host 发来的 frame 是对象；帧文本化以复用 handleFrame 的解析
    await handleFrame(JSON.stringify(frame ?? {}), impl)
  })
}
