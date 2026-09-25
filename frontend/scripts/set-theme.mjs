import http from 'node:http'

const PORT = 9333

function httpJson(p) {
  return new Promise((resolve, reject) => {
    http
      .get({ host: '127.0.0.1', port: PORT, path: p }, (res) => {
        let b = ''
        res.on('data', (c) => (b += c))
        res.on('end', () => {
          try {
            resolve(JSON.parse(b))
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
      const m = JSON.parse(ev.data)
      if (m.id && this.pending.has(m.id)) {
        const { resolve, reject } = this.pending.get(m.id)
        this.pending.delete(m.id)
        if (m.error) reject(new Error(JSON.stringify(m.error)))
        else resolve(m.result)
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
          reject(new Error(`timeout ${method}`))
        }
      }, 20000)
    })
  }
  async eval(expression) {
    const r = await this.send('Runtime.evaluate', {
      expression,
      returnByValue: true,
      awaitPromise: true
    })
    if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails))
    return r.result?.value
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

/** 只负责切主题/视图并输出状态，不做截图（截图由外部系统级工具完成）。 */
async function main() {
  const themeId = process.argv[2]
  const view = process.argv[3] || 'settings'
  const label = THEMES[themeId]

  const targets = await httpJson('/json')
  const page = targets.find((t) => t.type === 'page')
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((r) => ws.addEventListener('open', r))
  const cdp = new CDP(ws)
  await cdp.send('Runtime.enable')

  await cdp.eval(`
    (() => {
      const b = [...document.querySelectorAll('aside nav button')].find(x => x.textContent.trim() === ${JSON.stringify(VIEWS[view])});
      if (b) b.click();
      return true;
    })()
  `)
  await new Promise((r) => setTimeout(r, 350))

  const clicked = await cdp.eval(`
    (() => {
      const names = ['黑曜石','石墨','银白','暖白','原纸'];
      const cards = [...document.querySelectorAll('button')].filter(b => names.some(n => (b.textContent||'').includes(n)));
      const card = cards.find(c => (c.textContent||'').includes(${JSON.stringify(label)}));
      if (!card) return 'not-found';
      card.click();
      return 'clicked';
    })()
  `)
  await new Promise((r) => setTimeout(r, 400))

  const state = await cdp.eval(`
    (() => {
      const root = document.documentElement;
      const cs = getComputedStyle(root);
      return JSON.stringify({
        theme: root.getAttribute('data-theme'),
        scheme: root.getAttribute('data-color-scheme'),
        canvas: cs.getPropertyValue('--c-canvas').trim(),
        ink: cs.getPropertyValue('--c-ink').trim(),
        accent: cs.getPropertyValue('--c-accent').trim(),
        borderAlpha: cs.getPropertyValue('--c-border-alpha').trim()
      });
    })()
  `)
  console.log(`${themeId}: click=${clicked} ${state}`)
  ws.close()
}

main().catch((e) => {
  console.error('ERROR:', e.message)
  process.exit(1)
})
