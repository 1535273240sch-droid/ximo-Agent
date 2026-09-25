import http from 'node:http'

const PORT = 9333

function httpJson(path) {
  return new Promise((resolve, reject) => {
    http
      .get({ host: '127.0.0.1', port: PORT, path }, (res) => {
        let body = ''
        res.on('data', (c) => (body += c))
        res.on('end', () => {
          try {
            resolve(JSON.parse(body))
          } catch (err) {
            reject(err)
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
    if (res.exceptionDetails) {
      throw new Error(`page exception: ${JSON.stringify(res.exceptionDetails)}`)
    }
    return res.result?.value
  }
}

/**
 * 用真实点击切换主题：定位主题卡片按钮（卡片内含主题中文名 + 「Obsidian/Graphite」副标题），
 * 逐个精确匹配，避免误点。
 */
const THEME_LABELS = {
  obsidian: '黑曜石',
  graphite: '石墨',
  silver: '银白',
  'warm-white': '暖白',
  paper: '原纸'
}

async function main() {
  const themeId = process.argv[2]
  const view = process.argv[3] || 'settings'
  const label = THEME_LABELS[themeId]
  if (!label) {
    console.error('unknown theme:', themeId)
    process.exit(2)
  }

  const targets = await httpJson('/json')
  const page = targets.find((t) => t.type === 'page')
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((r) => ws.addEventListener('open', r))
  const cdp = new CDP(ws)
  await cdp.send('Runtime.enable')

  // 1. 先切到目标视图
  const viewLabels = { chat: '对话', experts: '专家', knowledge: '知识库', settings: '设置' }
  await cdp.eval(`
    (() => {
      const b = [...document.querySelectorAll('aside nav button')].find(x => x.textContent.trim() === ${JSON.stringify(viewLabels[view])});
      if (b) b.click();
      return 'ok';
    })()
  `)

  await new Promise((r) => setTimeout(r, 300))

  // 2. 在设置页精确点击主题卡片
  const clicked = await cdp.eval(`
    (() => {
      const cards = [...document.querySelectorAll('button')].filter(b => {
        const t = b.textContent || '';
        return t.includes('黑曜石') || t.includes('石墨') || t.includes('银白') || t.includes('暖白') || t.includes('原纸');
      });
      const card = cards.find(c => (c.textContent || '').includes(${JSON.stringify(label)}));
      if (!card) return 'card-not-found';
      card.click();
      return 'clicked:' + ${JSON.stringify(label)};
    })()
  `)

  await new Promise((r) => setTimeout(r, 400))

  const state = await cdp.eval(`
    (() => {
      const root = document.documentElement;
      const cs = getComputedStyle(root);
      return JSON.stringify({
        dataTheme: root.getAttribute('data-theme'),
        scheme: root.getAttribute('data-color-scheme'),
        inkVar: cs.getPropertyValue('--c-ink').trim(),
        canvasVar: cs.getPropertyValue('--c-canvas').trim(),
        bodyBg: getComputedStyle(document.body).backgroundColor,
        bodyColor: getComputedStyle(document.body).color,
        fontSize: getComputedStyle(document.body).fontSize,
        fontFamily: getComputedStyle(document.body).fontFamily.substring(0, 60),
        minTextPx: Math.min(...[...document.querySelectorAll('*')].map(e => parseFloat(getComputedStyle(e).fontSize)).filter(n => n > 0))
      });
    })()
  `)

  console.log(`click=${clicked}`)
  console.log(state)
  ws.close()
}

main().catch((e) => {
  console.error('ERROR:', e.message)
  process.exit(1)
})
