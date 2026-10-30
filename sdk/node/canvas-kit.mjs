// canvas-kit：给插件直接用的画布工具。
// 原生 @napi-rs/canvas 由运行时共享依赖提供，插件包不再打包。
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'

const require = createRequire(import.meta.url)
const { createCanvas, GlobalFonts, loadImage: loadImageRaw } = require('@napi-rs/canvas')
function log(level, message) { try { process.stderr.write(`[canvas-kit] ${level} ${message}\n`) } catch {} }

const __dirname = path.dirname(fileURLToPath(import.meta.url))
// 字体不归某个服务所有：扫所有插件的 assets/fonts、运行时 fonts、共享 fonts，
// 谁要画中文谁自己带字体（或者用运行时自带的霞鹜文楷），canvas 只负责注册成全局字体。
// 部署后本文件在 <运行区>/<插件>/_sdk/ 下，往上一级就是运行区（data/plugins）。
const RUNTIME_DIR = path.resolve(__dirname, '..', '..')
const SHARED_FONT_DIR = process.env.QIXIN_SHARED_DEPS_DIR
  ? path.join(process.env.QIXIN_SHARED_DEPS_DIR, 'fonts')
  : path.resolve(RUNTIME_DIR, '..', 'deps', 'fonts')
const RUNTIME_FONT_DIR = process.env.QIXIN_RUNTIME_DIR
  ? path.join(process.env.QIXIN_RUNTIME_DIR, 'fonts')
  : ''

let fontsLoaded = false
const imageCache = new Map()

// ---------------------------------------------------------------- 字体

// 可能的字体目录：<插件>/app/fonts、<插件>/fonts、<插件>/assets/font
const FONT_SUBDIRS = ['assets/fonts', 'assets/font', 'app/fonts', 'fonts']

function ensureFonts() {
  if (fontsLoaded) return
  try {
    if (!fs.existsSync(RUNTIME_DIR)) return
    for (const pluginDir of fs.readdirSync(RUNTIME_DIR, { withFileTypes: true })) {
      if (!pluginDir.isDirectory() || pluginDir.name.startsWith('.')) continue
      for (const sub of FONT_SUBDIRS) {
        const dir = path.join(RUNTIME_DIR, pluginDir.name, sub)
        if (!fs.existsSync(dir)) continue
        for (const file of fs.readdirSync(dir)) {
          if (!/\.(ttf|otf|ttc)$/i.test(file)) continue
          const alias = path.basename(file, path.extname(file))
          try {
            GlobalFonts.registerFromPath(path.join(dir, file), alias)
            log('debug', `画布：注册字体 ${alias}（来自 ${pluginDir.name}/${sub}）`)
          } catch (err) {
            log('warn', `画布：字体 ${file} 注册失败 ${err.message}`)
          }
        }
      }
    }
    if (RUNTIME_FONT_DIR && fs.existsSync(RUNTIME_FONT_DIR)) {
      for (const file of fs.readdirSync(RUNTIME_FONT_DIR)) {
        if (!/\.(ttf|otf|ttc)$/i.test(file)) continue
        try { GlobalFonts.registerFromPath(path.join(RUNTIME_FONT_DIR, file), path.basename(file, path.extname(file))) } catch {}
      }
    }
    if (SHARED_FONT_DIR && fs.existsSync(SHARED_FONT_DIR)) {
      for (const file of fs.readdirSync(SHARED_FONT_DIR)) {
        if (!/\.(ttf|otf|ttc)$/i.test(file)) continue
        try {
          GlobalFonts.registerFromPath(path.join(SHARED_FONT_DIR, file), path.basename(file, path.extname(file)))
          log('debug', `画布：注册共享字体 ${file}`)
        } catch {}
      }
    }
    fontsLoaded = true
  } catch (err) {
    log('warn', `画布：扫描字体失败 ${err.message}`)
  }
}

// ---------------------------------------------------------------- 图片

async function loadImageCached(src) {
  const isBuf = Buffer.isBuffer(src) || src instanceof Uint8Array
  if (!isBuf && imageCache.has(src)) return imageCache.get(src)
  let buffer
  if (isBuf) {
    // 直接给字节（插件自己读的文件、转换后的图）也认
    buffer = Buffer.from(src)
  } else if (String(src).startsWith('data:')) {
    buffer = Buffer.from(String(src).split(',')[1] || '', 'base64')
  } else if (String(src).startsWith('base64://')) {
    buffer = Buffer.from(String(src).slice(9), 'base64')
  } else if (String(src).startsWith('http')) {
    const resp = await fetch(src, { signal: AbortSignal.timeout(15000) })
    if (!resp.ok) throw new Error(`下载图片失败 HTTP ${resp.status}`)
    buffer = Buffer.from(await resp.arrayBuffer())
  } else {
    buffer = fs.readFileSync(src)
  }
  const img = await loadImageRaw(buffer)
  // 只缓存小图，避免内存涨太快（Buffer 每次都是新对象，缓存了也没用）
  if (!isBuf && buffer.length < 2 * 1024 * 1024) imageCache.set(src, img)
  return img
}

// ---------------------------------------------------------------- 绘制

function roundRect(ctx, x, y, w, h, r) {
  const radius = Math.min(r, w / 2, h / 2)
  ctx.beginPath()
  ctx.moveTo(x + radius, y)
  ctx.arcTo(x + w, y, x + w, y + h, radius)
  ctx.arcTo(x + w, y + h, x, y + h, radius)
  ctx.arcTo(x, y + h, x, y, radius)
  ctx.arcTo(x, y, x + w, y, radius)
  ctx.closePath()
}

// 框架兜底字体：插件不指定 font 时用它，保证中文不会变方块
const DEFAULT_FONT = 'LXGWWenKai'

function fontOf(op) {
  const size = Number(op.size) || 16
  const weight = op.bold ? 'bold ' : ''
  // 插件可以传自己的字体名，没有就用框架带的开源字体
  const family = op.font || `"${DEFAULT_FONT}"`
  return `${weight}${size}px ${family}`
}

// 按宽度折行（中文按字切）
function wrapText(ctx, text, maxWidth, maxLines) {
  const lines = []
  let current = ''
  for (const ch of String(text)) {
    if (ctx.measureText(current + ch).width > maxWidth && current) {
      lines.push(current)
      current = ch
      if (maxLines && lines.length >= maxLines) return lines
    } else {
      current += ch
    }
  }
  if (current) lines.push(current)
  return maxLines ? lines.slice(0, maxLines) : lines
}

async function renderOps({ width = 800, height = 600, background, ops = [], format = 'image/png', quality = 88 }) {
  ensureFonts()

  const canvas = createCanvas(Number(width) || 800, Number(height) || 600)
  const ctx = canvas.getContext('2d')

  if (background) {
    ctx.fillStyle = background
    ctx.fillRect(0, 0, canvas.width, canvas.height)
  }

  for (const op of ops) {
    switch (op.type) {
      case 'image': {
        const img = await loadImageCached(op.src)
        const x = Number(op.x) || 0
        const y = Number(op.y) || 0
        const w = Number(op.w) || img.width
        const h = Number(op.h) || img.height
        if (op.round) {
          ctx.save()
          roundRect(ctx, x, y, w, h, Number(op.round))
          ctx.clip()
          ctx.drawImage(img, x, y, w, h)
          ctx.restore()
        } else {
          ctx.drawImage(img, x, y, w, h)
        }
        break
      }

      case 'rect': {
        ctx.beginPath()
        const x = Number(op.x) || 0
        const y = Number(op.y) || 0
        const w = Number(op.w) || 0
        const h = Number(op.h) || 0
        if (op.radius) roundRect(ctx, x, y, w, h, Number(op.radius))
        else ctx.rect(x, y, w, h)
        if (op.fill) {
          ctx.fillStyle = op.fill
          ctx.fill()
        }
        if (op.stroke) {
          ctx.strokeStyle = op.stroke
          ctx.lineWidth = Number(op.lineWidth) || 1
          ctx.stroke()
        }
        break
      }

      case 'text': {
        ctx.font = fontOf(op)
        ctx.fillStyle = op.color || '#1d1d1f'
        ctx.textBaseline = op.baseline || 'alphabetic'
        ctx.textAlign = op.align || 'left'

        const text = String(op.text ?? '')
        if (op.vertical) {
          // 竖排：逐字往下画
          const size = Number(op.size) || 16
          const step = size + (Number(op.gap) || 4)
          const chars = [...text]
          for (let i = 0; i < chars.length; i++) {
            ctx.fillText(chars[i], Number(op.x) || 0, (Number(op.y) || 0) + i * step)
          }
        } else if (op.wrap) {
          const lines = wrapText(ctx, text, Number(op.wrap), Number(op.maxLines) || 0)
          const lineHeight = Number(op.lineHeight) || (Number(op.size) || 16) * 1.5
          for (let i = 0; i < lines.length; i++) {
            ctx.fillText(lines[i], Number(op.x) || 0, (Number(op.y) || 0) + i * lineHeight)
          }
        } else {
          ctx.fillText(text, Number(op.x) || 0, Number(op.y) || 0)
        }
        break
      }

      case 'ellipsis': {
        ctx.font = fontOf(op)
        ctx.fillStyle = op.color || '#1d1d1f'
        ctx.textBaseline = op.baseline || 'alphabetic'
        const maxWidth = Number(op.wrap) || 200
        let text = String(op.text ?? '')
        if (ctx.measureText(text).width > maxWidth) {
          while (text.length > 1 && ctx.measureText(text + '…').width > maxWidth) {
            text = text.slice(0, -1)
          }
          text += '…'
        }
        ctx.fillText(text, Number(op.x) || 0, Number(op.y) || 0)
        break
      }

      case 'measure': {
        // 给调用方量文字宽度用（返回结果在 measures 里）
        ctx.font = fontOf(op)
        const m = ctx.measureText(String(op.text ?? ''))
        op.__result = m.width
        break
      }

      default:
        break
    }
  }

  // 调用方能按次指定格式：周报这种大图用 JPEG 省 4 倍体积
  return canvas.toBuffer(format || 'image/png', (format || 'image/png') === 'image/jpeg' ? Number(quality) || 88 : undefined)
}

// ---------------------------------------------------------------- 导出

export {
  renderOps,
  ensureFonts,
  wrapText,
  roundRect,
  createCanvas,
  GlobalFonts,
}
// 名字直给：插件也可以直接拿 loadImage 做裁剪/合成（源矩形裁剪请用 @napi-rs/canvas）
export const loadImage = loadImageCached

export async function probe(urls) {
  const out = []
  for (const url of (Array.isArray(urls) ? urls : []).slice(0, 60)) {
    try {
      const img = await loadImageCached(url)
      out.push({ url, width: img.width, height: img.height })
    } catch {
      out.push({ url, width: 0, height: 0 })
    }
  }
  return out
}
