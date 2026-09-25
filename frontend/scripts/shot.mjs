import http from 'node:http'
import fs from 'node:fs'
import path from 'node:path'

const PORT = 9333

function httpJson(p) {
  return new Promise((resolve, reject) => {
    http
      .get({ host: '127.0.0.1', port: PORT, path: p }, (res) => {
        let body = ''
        res.on('data', (c) => (body += c))
        res.on('end', () => {
          try {
            resolve(JSON.parse(body))
          } catch (e) {
            reject(e)
          }
        })
      })
      .on('error', reject)
  })
}

class CDP {
  constructor(ws) {
    this.ws = ws
    this.id = 0
    this.pending = new Map()
    ws.addEventListener('message', (ev) => {
      const msg = JSON.parse(ev.data)
      if (msg.id && this.pending.has(msg.id)) {
        const { resolve, reject } = this.pending.get(msg.id)
        this.pending.delete(msg.id)
        if (msg.error) reject(new Error(JSON.stringify(msg.error)))
        else resolve(msg.result)
      }
    })
  }
  send(method, params = {}) {
    const id = ++this.id
    return new Promise((resolve, reject) => {
      this.pending.set(id, { resolve, reject })
      this.ws.send(JSON.stringify({ id, method, params }))
      setTimeout(() => {
        if (this.pending.has(id)) {
          this.pending.delete(id)
          reject(new Error(`CDP ${method} timed out`))
        }
      }, 30000)
    })
  }
  async eval(expression) {
    const res = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true
    })
    if (res.exceptionDetails) throw new Error(JSON.stringify(res.exceptionDetails))
    return res.result?.value
  }
}

const THEMES = {
  obsidian: '黑曜石',
  graphite: '石墨',
  silver: '银白',
  'warm-white': '暖白',
  paper: '原纸'
}
const VIEWS = { chat: '对话', experts: '专家', knowledge: '知识库', settings: '设置' }

async function main() {
  const themeId = process.argv[2] || 'obsidian'
  const view = process.argv[3] || 'settings'
  const out = process.argv[4] || `shot-${themeId}-${view}.png`

  const targets = await httpJson('/json')
  const page = targets.find((t) => t.type === 'page')
  if (!page) throw new Error('no page target')

  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((r) => ws.addEventListener('open', r))
  const cdp = new CDP(ws)
  await cdp.send('Runtime.enable')
  await cdp.send('Page.enable')

  // 切视图
  await cdp.eval(`
    (() => {
      const b = [...document.querySelectorAll('aside nav button')].find(x => x.textContent.trim() === ${JSON.stringify(VIEWS[view])});
      if (b) b.click();
      return true;
    })()
  `)
  await new Promise((r) => setTimeout(r, 400))

  // 切主题（设置页里精确点击卡片）
  await cdp.eval(`
    (() => {
      const cards = [...document.querySelectorAll('button')].filter(b => {
        const t = b.textContent || '';
        return Object.values(['黑曜石','石墨','银白','暖白','原纸']).some(n => t.includes(n));
      });
      const card = cards.find(c => (c.textContent || '').includes(${JSON.stringify(THEMES[themeId])}));
      if (card) card.click();
      return Boolean(card);
    })()
  `)
  await new Promise((r) => setTimeout(r, 500))

  // 抓取渲染内容（不受窗口遮挡影响）
  const shot = await cdp.send('Page.captureScreenshot', {
    format: 'png',
    captureBeyondViewport: false
  })

  const buf = Buffer.from(shot.data, 'base64')
  const outPath = path.resolve(out)
  fs.writeFileSync(outPath, buf)
  console.log(`saved ${outPath} (${buf.length} bytes)`)
  ws.close()
}

main().catch((e) => {
  console.error('ERROR:', e.message)
  process.exit(1)
})
