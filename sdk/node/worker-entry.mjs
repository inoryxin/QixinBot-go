// 共享宿主的 worker 入口：按环境变量加载对应插件的 app 入口。
// 环境由 multi-host 按"每插件单独注入"（QIXIN_WORKER/QIXIN_PLUGIN_ID/
// QIXIN_PLUGIN_DIR/QIXIN_ENTRY），插件自己的 _sdk/qixin.mjs 检测到
// QIXIN_WORKER=1 后走 parentPort 传输。
import { pathToFileURL } from 'node:url'
import { join } from 'node:path'
import { parentPort } from 'node:worker_threads'

const dir = process.env.QIXIN_PLUGIN_DIR
if (!dir) throw new Error('worker 缺少 QIXIN_PLUGIN_DIR')
const entry = process.env.QIXIN_ENTRY || 'app/index.js'

// M0 内存自报：每 15s 向 multi-host 报本 isolate 的内存画像。
// rss 在 worker 里是进程级数字（参考值）；heapUsed/external/arrayBuffers
// 是本 isolate 的，可用于按插件归因。
setInterval(() => {
  if (!parentPort) return
  const m = process.memoryUsage()
  try {
    parentPort.postMessage({
      __mem: true,
      rss: m.rss,
      heapUsed: m.heapUsed,
      external: m.external,
      arrayBuffers: m.arrayBuffers || 0,
    })
  } catch {}
}, 15000)

await import(pathToFileURL(join(dir, entry)).href)
