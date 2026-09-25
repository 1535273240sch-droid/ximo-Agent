/**
 * Go 后端进程的生命周期管理。
 *
 * 前端不自己实现 Agent 逻辑：真正的引擎是那个 Go 可执行文件。主进程负责把它
 * 作为子进程拉起、等它就绪、并在退出时收干净，对外只暴露"后端可用了"这一件事。
 */

import { spawn, type ChildProcess } from 'node:child_process'
import fs from 'node:fs'
import net from 'node:net'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import type { BackendStatus } from '../shared/types'

/** supervisor 监听的基础端口；被占用时向后找一个空闲的。 */
const BASE_PORT = 45871

export interface BackendHandle {
  proc: ChildProcess
  endpoint: string
  port: number
}

export class BackendManager {
  private handle: BackendHandle | null = null
  private status: BackendStatus = { kind: 'stopped' }
  private statusHandlers = new Set<(s: BackendStatus) => void>()

  onStatus(handler: (s: BackendStatus) => void): () => void {
    this.statusHandlers.add(handler)
    handler(this.status)
    return () => this.statusHandlers.delete(handler)
  }

  private setStatus(s: BackendStatus): void {
    this.status = s
    for (const h of this.statusHandlers) {
      try {
        h(s)
      } catch {
        // 订阅者异常不影响进程管理
      }
    }
  }

  getStatus(): BackendStatus {
    return this.status
  }

  /**
   * 定位后端可执行文件。
   *
   * 两个位置都要找，因为开发态与打包态布局完全不同：
   *  - 打包后：electron-builder 把 exe 放到 resources/backend/
   *  - 开发态：直接用 Go 仓库里刚编译出来的 dist/ximo-agent.exe
   */
  private resolveExecutable(): string | null {
    const candidates: string[] = []

    if (process.resourcesPath) {
      candidates.push(path.join(process.resourcesPath, 'backend', 'ximo-agent.exe'))
    }

    const here = path.dirname(fileURLToPath(import.meta.url))
    // 开发态：out/main/ 的上三级即 frontend/，再上一级是 Go 仓库根。
    candidates.push(path.resolve(here, '..', '..', '..', 'dist', 'ximo-agent.exe'))
    candidates.push(path.resolve(here, '..', '..', '..', '..', 'dist', 'ximo-agent.exe'))
    candidates.push(path.resolve(process.cwd(), '..', 'dist', 'ximo-agent.exe'))
    candidates.push(path.resolve(process.cwd(), 'dist', 'ximo-agent.exe'))

    for (const c of candidates) {
      try {
        if (fs.existsSync(c)) return c
      } catch {
        // 忽略无权限或非法路径
      }
    }
    return null
  }

  /** 端口是否空闲，避免与上一次没退干净的进程冲突。 */
  private isPortFree(port: number): Promise<boolean> {
    return new Promise((resolve) => {
      const srv = net.createServer()
      srv.once('error', () => resolve(false))
      srv.once('listening', () => {
        srv.close(() => resolve(true))
      })
      srv.listen(port, '127.0.0.1')
    })
  }

  private async pickPort(): Promise<number> {
    // 步进 2 且要求 p 与 p+1 同时空闲：supervisor 与 engine 各占一个端口，
    // 只检查 p 会选中与 engine 相邻的端口引发冲突。
    for (let p = BASE_PORT; p < BASE_PORT + 50; p += 2) {
      if (await this.isPortFree(p) && (await this.isPortFree(p + 1))) return p
    }
    throw new Error('no free port found for the backend in range 45871-45920')
  }

  /**
   * 启动后端并等待端口可连接。
   *
   * 只起 supervisor：它会自己拉起 engine 子进程，并负责崩溃重启与恢复下发。
   * 前端只跟 supervisor 说话，不直接碰 engine。
   */
  async start(): Promise<BackendStatus> {
    if (this.handle) return this.status

    this.setStatus({ kind: 'starting' })

    const exe = this.resolveExecutable()
    if (!exe) {
      const msg =
        '未找到后端可执行文件。请先在仓库根目录运行 build.cmd exe 生成 dist/ximo-agent.exe。'
      this.setStatus({ kind: 'error', message: msg })
      return this.status
    }

    let port: number
    try {
      port = await this.pickPort()
    } catch (err) {
      this.setStatus({ kind: 'error', message: (err as Error).message })
      return this.status
    }

    const endpoint = `tcp://127.0.0.1:${port}`
    this.setStatus({ kind: 'connecting' })

    const proc = spawn(exe, ['--role=supervisor', `--ipc-endpoint=${endpoint}`], {
      // 迁移脚本必须能被后端找到：它按可执行文件同级目录查找 migrations/，
      // 所以 cwd 设成 exe 所在目录，dist/ 与 resources/backend/ 两种布局都成立。
      cwd: path.dirname(exe),
      stdio: ['ignore', 'pipe', 'pipe'],
      windowsHide: true
    })

    proc.stdout?.on('data', (b: Buffer) => {
      process.stdout.write(`[backend] ${b.toString()}`)
    })
    proc.stderr?.on('data', (b: Buffer) => {
      process.stderr.write(`[backend] ${b.toString()}`)
    })
    proc.on('exit', (code, signal) => {
      this.handle = null
      if (this.status.kind !== 'stopped') {
        this.setStatus({
          kind: 'error',
          message: `后端进程意外退出 (code=${code}, signal=${signal})`
        })
      }
    })

    this.handle = { proc, endpoint, port }

    try {
      await this.waitForPort(port, 20_000)
    } catch (err) {
      this.setStatus({ kind: 'error', message: (err as Error).message })
      await this.stop()
      return this.status
    }

    this.setStatus({ kind: 'ready', pid: proc.pid ?? -1, endpoint })
    return this.status
  }

  /** 轮询端口直到可连接或超时。 */
  private waitForPort(port: number, timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs
    return new Promise((resolve, reject) => {
      const attempt = (): void => {
        if (Date.now() > deadline) {
          reject(new Error(`后端在 ${timeoutMs}ms 内未就绪`))
          return
        }
        const sock = net.createConnection({ host: '127.0.0.1', port })
        sock.once('connect', () => {
          sock.destroy()
          resolve()
        })
        sock.once('error', () => {
          sock.destroy()
          setTimeout(attempt, 250)
        })
      }
      attempt()
    })
  }

  /** 停止后端。先温和请求退出，超时再强杀；Windows 下强杀整个进程树防孤儿。 */
  async stop(): Promise<void> {
    const h = this.handle
    this.handle = null
    if (!h) {
      this.setStatus({ kind: 'stopped' })
      return
    }

    const proc = h.proc
    if (proc.exitCode === null && !proc.killed) {
      // Windows 必须先做整树强杀：supervisor 收到 SIGTERM 会立即退出，
      // 它拉起的 engine 子进程随即失去父进程，之后的 taskkill /T 因目标
      // 进程已死而杀不到引擎，留下持有数据库与端口的孤儿进程。
      if (process.platform === 'win32' && proc.pid) {
        try {
          spawn('taskkill', ['/pid', String(proc.pid), '/T', '/F'])
          // 树杀已覆盖 supervisor 与 engine，无需再走温和退出路径。
          this.setStatus({ kind: 'stopped' })
          return
        } catch {
          // taskkill 不可用时退回 SIGTERM 路径
        }
      }
      proc.kill('SIGTERM')
      const exited = await new Promise<boolean>((resolve) => {
        const t = setTimeout(() => resolve(false), 3_000)
        proc.once('exit', () => {
          clearTimeout(t)
          resolve(true)
        })
      })
      if (!exited && proc.exitCode === null) {
        proc.kill('SIGKILL')
      }
    }
    this.setStatus({ kind: 'stopped' })
  }

  getEndpoint(): string | null {
    return this.handle?.endpoint ?? null
  }
}
