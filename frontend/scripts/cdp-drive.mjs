/**
 * 通过 CDP 驱动正在运行的 Electron 窗口，用于主题与界面的可视化验收。
 *
 * 为什么需要这个脚本：Electron 是原生窗口，普通浏览器自动化工具驱动不了；
 * 但它的渲染进程本身是 Chromium，开了 --remote-debugging-port 之后就能用 CDP
 * 像操作网页一样操作它。这是验证"两套主题真的渲染出来了"的最直接手段。
 *
 * 用法：
 *   node scripts/cdp-drive.mjs '<javascript expression>'
 * 或使用内置场景：
 *   node scripts/cdp-drive.mjs --scenario obsidian-chat
 *
 * 依赖 Node 22 内置的 WebSocket。
 */

import http from 'node:http'

const PORT = Number(process.env.CDP_PORT || 9333)

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

/** 通过点击导航按钮切换视图（不依赖内部 store，等价于真实用户操作）。 */
function clickNav(label) {
  return `
    (() => {
      const btns = [...document.querySelectorAll('button')];
      const t = btns.find(b => b.textContent.trim() === ${JSON.stringify('')} + ${JSON.stringify(label)});
      if (!t) return 'nav-not-found:' + ${JSON.stringify(label)};
      t.click();
      return 'clicked:' + ${JSON.stringify(label)};
    })()
  `
}

/** 通过点击主题卡片切换主题（真实用户路径）。 */
function clickTheme(nameFragment) {
  return `
    (() => {
      const btns = [...document.querySelectorAll('button')];
      const t = btns.find(b => b.textContent.includes(${JSON.stringify(nameFragment)}) && b.textContent.includes('黑曜石') === false ? false : b.textContent.includes(${JSON.stringify(nameFragment)}));
      if (!t) return 'theme-not-found:' + ${JSON.stringify(nameFragment)};
      t.click();
      return 'clicked-theme:' + ${JSON.stringify(nameFragment)};
    })()
  `
}

const SCENARIOS = {
  'obsidian-chat': `(() => { const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='对话'); b&&b.click(); return 'chat'; })()`,
  'experts': `(() => { const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='专家'); b&&b.click(); return 'experts'; })()`,
  'knowledge': `(() => { const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='知识库'); b&&b.click(); return 'knowledge'; })()`,
  'settings': `(() => { const b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='设置'); b&&b.click(); return 'settings'; })()`,
  'theme-parchment': `(() => { const b=[...document.querySelectorAll('button')].find(x=>x.textContent.includes('羊皮卷')&&x.textContent.includes('羊皮卷，')); const c=[...document.querySelectorAll('button')].filter(x=>x.textContent.includes('羊皮卷'))[0]; if(!c) return 'nf'; c.click(); return 'parchment'; })()`,
  'theme-obsidian': `(() => { const c=[...document.querySelectorAll('button')].filter(x=>x.textContent.includes('黑曜石'))[0]; if(!c) return 'nf'; c.click(); return 'obsidian'; })()`,
  'report': `(() => {
     const root = document.documentElement;
     const cs = getComputedStyle(root);
     return JSON.stringify({
       theme: root.getAttribute('data-theme'),
       canvasVar: cs.getPropertyValue('--c-canvas').trim(),
       accentVar: cs.getPropertyValue('--c-accent').trim(),
       inkVar: cs.getPropertyValue('--c-ink').trim(),
       buttons: document.querySelectorAll('button').length,
       bodyBg: getComputedStyle(document.body).backgroundColor,
       glassCount: document.querySelectorAll('.glass-panel,.glass-raised,.glass-sidebar,.glass-inset').length
     });
  })()`
}

async function main() {
  const arg = process.argv.slice(2)
  let expr
  if (arg[0] === '--scenario') {
    expr = SCENARIOS[arg[1]]
    if (!expr) {
      console.error(`unknown scenario: ${arg[1]}`)
      console.error(`available: ${Object.keys(SCENARIOS).join(', ')}`)
      process.exit(2)
    }
  } else {
    expr = arg.join(' ')
  }
  if (!expr) {
    console.error('usage: node scripts/cdp-drive.mjs --scenario <name> | "<js expression>"')
    process.exit(2)
  }

  const targets = await httpJson('/json')
  const page = targets.find((t) => t.type === 'page')
  if (!page) {
    console.error('no page target; is the app running with --remote-debugging-port=' + PORT + '?')
    process.exit(1)
  }

  const ws = new WebSocket(page.webSocketDebuggerUrl)
  await new Promise((resolve, reject) => {
    ws.addEventListener('open', resolve)
    ws.addEventListener('error', reject)
  })

  const cdp = new CDP(ws)
  await cdp.send('Runtime.enable')
  const out = await cdp.eval(expr)
  console.log(typeof out === 'string' ? out : JSON.stringify(out, null, 2))
  ws.close()
}

main().catch((err) => {
  console.error('ERROR:', err.message)
  process.exit(1)
})
