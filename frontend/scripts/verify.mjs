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
      }, 40000)
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

/**
 * 直接通过渲染进程的 preload 桥验证「密钥写入」与「模型获取」这两条真实链路。
 * 这不是点界面按钮，而是走同一套 window.ximo.* 接口——如果它通，
 * 界面按钮就一定通（按钮只是调用同样的方法）。
 */
async function main() {
  const cmd = process.argv[2]

  const targets = await httpJson('/json')
  const page = targets.find((t) => t.type === 'page')
  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((r) => ws.addEventListener('open', r))
  const cdp = new CDP(ws)
  await cdp.send('Runtime.enable')

  if (cmd === 'secret-status') {
    const out = await cdp.eval(`(async () => {
      const s = await window.ximo.secretStatus();
      return JSON.stringify(s);
    })()`)
    console.log('secretStatus =>', out)
  }

  if (cmd === 'put-secret') {
    const value = process.argv[3] || 'sk-test-key-for-verification-only'
    const out = await cdp.eval(`(async () => {
      try {
        const r = await window.ximo.putSecret(${JSON.stringify(value)});
        return JSON.stringify({ ok: true, result: r });
      } catch (e) {
        return JSON.stringify({ ok: false, error: String(e && e.message || e) });
      }
    })()`)
    console.log('putSecret =>', out)
  }

  if (cmd === 'list-models') {
    const out = await cdp.eval(`(async () => {
      try {
        const r = await window.ximo.listModels();
        return JSON.stringify({ ok: true, base: r.base_url, count: (r.models||[]).length, error: r.error || null, first: (r.models||[]).slice(0,3) });
      } catch (e) {
        return JSON.stringify({ ok: false, error: String(e && e.message || e) });
      }
    })()`)
    console.log('listModels =>', out)
  }

  if (cmd === 'settings') {
    const out = await cdp.eval(`(async () => {
      const s = await window.ximo.getSettings();
      return JSON.stringify(s);
    })()`)
    console.log('getSettings =>', out)
  }

  if (cmd === 'theme') {
    const out = await cdp.eval(`(() => {
      const root = document.documentElement;
      const cs = getComputedStyle(root);
      return JSON.stringify({
        theme: root.getAttribute('data-theme'),
        scheme: root.getAttribute('data-color-scheme'),
        canvas: cs.getPropertyValue('--c-canvas').trim(),
        ink: cs.getPropertyValue('--c-ink').trim(),
        borderAlpha: cs.getPropertyValue('--c-border-alpha').trim()
      });
    })()`)
    console.log('theme =>', out)
  }

  ws.close()
}

main().catch((e) => {
  console.error('ERROR:', e.message)
  process.exit(1)
})
