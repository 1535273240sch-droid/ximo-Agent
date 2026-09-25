/**
 * 与 Go 后端进程的帧协议客户端。
 *
 * 对应实现：internal/ipc/client.go + internal/ipcapi/service.go 的 Client。
 *
 * 职责：
 *   1. 建立 TCP 连接（Go 侧 supervisor 监听 tcp://127.0.0.1:<port>）
 *   2. 发送业务请求帧并按 RequestID 配对响应
 *   3. 接收服务端主动推送的帧并派发给订阅者
 *   4. 断线自动重连（指数退避）
 *
 * 为什么主进程自己做而不用 Go 的 ipcapi 客户端：Electron 主进程是 Node 环境，
 * 无法调用 Go 代码；而前后端必须通过真实的进程间字节流通信（这正是本轮要验证
 * 的"前后端真的能对话"）。所以这里按 Go 的协议做了一份等价实现。
 */

import net from 'node:net'
import {
  assembleFrame,
  decodeEnvelope,
  decodeHeader,
  encodeEnvelope,
  encodeFrame,
  FIXED_HEADER_SIZE,
  FrameType,
  type Envelope,
  type Frame
} from '../shared/frame'
import type { DurableEvent } from '../shared/types'

/** 请求超时（毫秒）。Go 侧默认 15s，这里给稍长的余量。 */
const DEFAULT_TIMEOUT_MS = 20_000
/** 重连退避序列（毫秒）。 */
const RECONNECT_BACKOFF = [200, 500, 1000, 2000, 5000, 10000]

type PendingRequest = {
  resolve: (frame: Frame) => void
  reject: (err: Error) => void
  timer: NodeJS.Timeout
}

export type ConnectionState = 'idle' | 'connecting' | 'ready' | 'reconnecting' | 'closed'

export class BackendClient {
  private socket: net.Socket | null = null
  private buffer: Buffer = Buffer.alloc(0)
  private pending = new Map<string, PendingRequest>()
  private subscribers = new Map<string, Set<(frame: Frame) => void>>()
  private eventHandlers = new Set<(ev: DurableEvent) => void>()
  private stateHandlers = new Set<(s: ConnectionState) => void>()

  private seq = 0
  private reqCounter = 0
  private state: ConnectionState = 'idle'
  private reconnectAttempt = 0
  private reconnectTimer: NodeJS.Timeout | null = null
  private closedByUser = false

  constructor(
    private readonly host: string,
    private readonly port: number
  ) {}

  // -------------------------------------------------------------------------
  // 连接管理
  // -------------------------------------------------------------------------

  /** 建立连接；失败时自动进入重连。 */
  async connect(): Promise<void> {
    // 已就绪的连接直接复用：ensureBackend 的重试循环会反复调用 connect，
    // 若每次都重建 socket，健康连接会被无谓拆掉，还会泄漏旧 socket。
    if (this.socket && this.state === 'ready') {
      return
    }
    this.closedByUser = false
    if (this.socket) {
      try {
        this.socket.destroy()
      } catch {
        // 忽略销毁失败
      }
      this.socket = null
    }
    return this.open()
  }

  private open(): Promise<void> {
    this.setState(this.reconnectAttempt === 0 ? 'connecting' : 'reconnecting')

    return new Promise((resolve, reject) => {
      const socket = net.createConnection({ host: this.host, port: this.port })
      this.socket = socket
      socket.setNoDelay(true)

      const onConnectError = (err: Error): void => {
        socket.destroy()
        this.socket = null
        if (this.reconnectAttempt === 0) {
          reject(err)
        }
        this.scheduleReconnect()
      }

      socket.once('error', onConnectError)
      socket.once('connect', () => {
        socket.off('error', onConnectError)
        socket.on('error', (err) => this.handleSocketError(err))
        socket.on('close', () => this.handleSocketClose())
        socket.on('data', (chunk: Buffer) => this.onData(chunk))
        this.reconnectAttempt = 0
        this.setState('ready')
        resolve()
      })
    })
  }

  private handleSocketError(err: Error): void {
    // 连接层错误：所有在飞请求都必须失败，否则调用方会永久挂起。
    this.failAllPending(`connection error: ${err.message}`)
  }

  private handleSocketClose(): void {
    this.socket = null
    this.failAllPending('connection closed')
    if (this.closedByUser) {
      this.setState('closed')
      return
    }
    this.scheduleReconnect()
  }

  private scheduleReconnect(): void {
    if (this.closedByUser || this.reconnectTimer) return
    const delay = RECONNECT_BACKOFF[Math.min(this.reconnectAttempt, RECONNECT_BACKOFF.length - 1)]
    this.reconnectAttempt++
    this.setState('reconnecting')
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.open().catch(() => {
        // open() 内部已经安排了下一次重连，这里无需处理。
      })
    }, delay)
  }

  /** 主动关闭，不再重连。 */
  close(): void {
    this.closedByUser = true
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.failAllPending('client closed')
    if (this.socket) {
      this.socket.destroy()
      this.socket = null
    }
    this.setState('closed')
  }

  getState(): ConnectionState {
    return this.state
  }

  onStateChange(handler: (s: ConnectionState) => void): () => void {
    this.stateHandlers.add(handler)
    handler(this.state)
    return () => this.stateHandlers.delete(handler)
  }

  private setState(s: ConnectionState): void {
    this.state = s
    for (const h of this.stateHandlers) {
      try {
        h(s)
      } catch {
        // 订阅者异常不能影响连接本身。
      }
    }
  }

  private failAllPending(reason: string): void {
    for (const [id, p] of this.pending) {
      clearTimeout(p.timer)
      p.reject(new Error(reason))
      this.pending.delete(id)
    }
  }

  // -------------------------------------------------------------------------
  // 收帧：流式切分
  // -------------------------------------------------------------------------

  /**
   * 处理字节流。
   *
   * TCP 不保证一次 data 事件对应一帧，所以必须自己缓冲并切分：
   * 先攒够 29 字节定长头 → 读出变长元信息长度 → 攒够元信息 → 按 payloadLength
   * 攒够载荷 → 组装成一帧。
   */
  private onData(chunk: Buffer): void {
    this.buffer = Buffer.concat([this.buffer, chunk])

    for (;;) {
      if (this.buffer.length < FIXED_HEADER_SIZE) return

      const fixed = new Uint8Array(
        this.buffer.buffer,
        this.buffer.byteOffset,
        FIXED_HEADER_SIZE
      )

      let metaLength: number
      let payloadLength: number
      try {
        const decoded = decodeHeader(fixed)
        metaLength = decoded.metaLength
        payloadLength = decoded.header.payloadLength
      } catch (err) {
        // 头部非法说明字节流已经错位，继续读只会读到更多垃圾：断开重连。
        this.handleFatalFrameError(err)
        return
      }

      const total = FIXED_HEADER_SIZE + metaLength + payloadLength
      if (this.buffer.length < total) return

      const meta = new Uint8Array(
        this.buffer.buffer,
        this.buffer.byteOffset + FIXED_HEADER_SIZE,
        metaLength
      )
      const payload = new Uint8Array(
        this.buffer.buffer,
        this.buffer.byteOffset + FIXED_HEADER_SIZE + metaLength,
        payloadLength
      )

      let frame: Frame
      try {
        frame = assembleFrame(fixed, meta, payload)
      } catch (err) {
        this.handleFatalFrameError(err)
        return
      }

      this.buffer = this.buffer.subarray(total)
      this.dispatch(frame)
    }
  }

  private handleFatalFrameError(err: unknown): void {
    this.buffer = Buffer.alloc(0)
    this.failAllPending(`protocol error: ${(err as Error).message}`)
    if (this.socket) {
      this.socket.destroy()
      this.socket = null
    }
    if (!this.closedByUser) this.scheduleReconnect()
  }

  private dispatch(frame: Frame): void {
    const { requestId, type } = frame.header

    // 1. 先看是否有人在等这个 RequestID 的响应。
    if (requestId) {
      const p = this.pending.get(requestId)
      if (p) {
        clearTimeout(p.timer)
        this.pending.delete(requestId)
        p.resolve(frame)
        return
      }
    }

    // 2. 心跳等系统帧直接吞掉（后端会定期 Ack，不需要上层关心）。
    if (type === FrameType.HeartbeatAck || type === FrameType.Pong) return

    // 3. 事件推送：直接转成 DurableEvent 交给订阅者。
    if (type === FrameType.EventStream) {
      this.emitEvents(frame)
      return
    }

    // 4. 其余按类型派发给订阅者。
    const subs = this.subscribers.get(type)
    if (subs) {
      for (const h of subs) {
        try {
          h(frame)
        } catch {
          // 单个订阅者异常不影响其他订阅者。
        }
      }
    }
  }

  private emitEvents(frame: Frame): void {
    try {
      const env: Envelope<{ events?: DurableEvent[] }> = decodeEnvelope(frame.payload)
      if (!env.ok || !env.data?.events) return
      for (const ev of env.data.events) {
        for (const h of this.eventHandlers) {
          try {
            h(ev)
          } catch {
            // 同上
          }
        }
      }
    } catch {
      // 事件帧解析失败不致命：丢弃这一批，下一批照常。
    }
  }

  // -------------------------------------------------------------------------
  // 发帧
  // -------------------------------------------------------------------------

  /** 发送一个请求并等待同 RequestID 的响应。 */
  async request<T>(type: string, payload: unknown, timeoutMs = DEFAULT_TIMEOUT_MS): Promise<T> {
    const socket = this.socket
    if (!socket || this.state !== 'ready') {
      throw new Error(`backend not connected (state=${this.state})`)
    }

    const requestId = `${type}#${++this.reqCounter}`
    const frame: Frame = {
      header: {
        version: 1,
        requestId,
        sessionId: '',
        sequence: ++this.seq,
        type,
        payloadLength: 0,
        deadlineMs: Date.now() + timeoutMs
      },
      payload: encodeEnvelope(payload)
    }

    const encoded = encodeFrame(frame)

    const response = new Promise<Frame>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(requestId)
        reject(new Error(`request ${type} timed out after ${timeoutMs}ms`))
      }, timeoutMs)
      this.pending.set(requestId, { resolve, reject, timer })
    })

    socket.write(Buffer.from(encoded.buffer, encoded.byteOffset, encoded.byteLength))

    const respFrame = await response
    const env = decodeEnvelope<T>(respFrame.payload)
    if (!env.ok) {
      throw new Error(env.error || `${type} failed`)
    }
    return env.data as T
  }

  /** 发送一个不需要响应的帧（通知类）。 */
  send(type: string, payload: unknown): void {
    const socket = this.socket
    if (!socket || this.state !== 'ready') return
    const frame: Frame = {
      header: {
        version: 1,
        requestId: '',
        sessionId: '',
        sequence: ++this.seq,
        type,
        payloadLength: 0,
        deadlineMs: 0
      },
      payload: encodeEnvelope(payload)
    }
    const encoded = encodeFrame(frame)
    socket.write(Buffer.from(encoded.buffer, encoded.byteOffset, encoded.byteLength))
  }

  // -------------------------------------------------------------------------
  // 业务方法：与 ipcapi.Client 一一对应
  // -------------------------------------------------------------------------

  submit(payload: unknown): Promise<unknown> {
    return this.request(FrameType.RunSubmit, payload)
  }

  cancel(runId: string): Promise<unknown> {
    return this.request(FrameType.RunCancel, { run_id: runId })
  }

  status(runId: string): Promise<unknown> {
    return this.request(FrameType.RunStatus, { run_id: runId })
  }

  events(runId: string, afterSeq: number): Promise<unknown> {
    return this.request(FrameType.EventStream, { run_id: runId, after_seq: afterSeq })
  }

  resume(runId: string): Promise<unknown> {
    return this.request(FrameType.RunResume, { run_id: runId })
  }

  /** 确认或否决一个 run 已提出的执行计划（任务4）。 */
  confirmPlan(runId: string, approved: boolean): Promise<unknown> {
    return this.request(FrameType.PlanConfirm, { run_id: runId, approved })
  }

  /** 探活。 */
  ping(): Promise<void> {
    return this.request(FrameType.Ping, {}, 5000).then(() => undefined)
  }

  /** 查询密钥配置状态（不含明文）。 */
  secretStatus(): Promise<unknown> {
    return this.request(FrameType.SecretStatus, {})
  }

  /** 写入密钥，返回安全存储中的引用。 */
  putSecret(value: string): Promise<unknown> {
    return this.request(FrameType.SecretPut, { value })
  }

  /** 读取运行时配置。 */
  getSettings(): Promise<unknown> {
    return this.request(FrameType.ConfigGet, {})
  }

  /** 提交运行时配置。 */
  applySettings(payload: unknown): Promise<unknown> {
    return this.request(FrameType.ConfigSet, payload)
  }

  /** 查询服务商可用模型。base_url 非空时按该地址查询（表单当前值）。 */
  listModels(opts?: { base_url?: string }): Promise<unknown> {
    return this.request(FrameType.ModelList, { base_url: opts?.base_url ?? '' }, 30_000)
  }

  onEvent(handler: (ev: DurableEvent) => void): () => void {
    this.eventHandlers.add(handler)
    return () => this.eventHandlers.delete(handler)
  }

  subscribe(type: string, handler: (frame: Frame) => void): () => void {
    let set = this.subscribers.get(type)
    if (!set) {
      set = new Set()
      this.subscribers.set(type, set)
    }
    set.add(handler)
    return () => set?.delete(handler)
  }
}
