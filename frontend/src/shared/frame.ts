/**
 * Go 侧 IPC 帧协议的 TypeScript 复刻。
 *
 * 对应实现：internal/ipc/protocol.go 的 WriteFrame / ReadFrame。
 *
 * 为什么必须逐字节复刻而不是"大致模仿"：Electron 主进程要用它和 Go 后端进程
 * 直接通信，双方对同一段字节流的解释必须完全一致。任何字段宽度、字节序或
 * 偏移量的偏差都不会报错，而是表现为"读到的 Type 是乱码"或"连接直接断开"，
 * 极难排查。因此这里把 Go 侧的布局注释一并搬过来作为对照。
 *
 * Wire 编码布局（共 29 字节定长头，BigEndian）：
 *   [0..1]   Magic (0x58, 0x4D) == "XM"
 *   [2]      Version (1 字节)      当前 = 1
 *   [3..4]   TypeLen (2 字节)
 *   [5..6]   ReqIDLen (2 字节)
 *   [7..8]   SessIDLen (2 字节)
 *   [9..16]  Sequence (8 字节)
 *   [17..24] DeadlineMs (8 字节, 有符号，0 表示无截止时间)
 *   [25..28] PayloadLength (4 字节)
 *   [29..]   Type 字节 | RequestID 字节 | SessionID 字节 | Payload 字节
 */

export const FRAME_MAGIC_0 = 0x58 // 'X'
export const FRAME_MAGIC_1 = 0x4d // 'M'
export const FRAME_VERSION = 1
export const FIXED_HEADER_SIZE = 29

export interface FrameHeader {
  version: number
  requestId: string
  sessionId: string
  sequence: number
  type: string
  payloadLength: number
  /** 毫秒级 Unix 截止时间戳；0 表示无截止时间。 */
  deadlineMs: number
}

export interface Frame {
  header: FrameHeader
  payload: Uint8Array
}

/** 帧类型常量，与 internal/ipc/protocol.go 的 Type* 常量一一对应。 */
export const FrameType = {
  Heartbeat: 'system.heartbeat',
  HeartbeatAck: 'system.heartbeat_ack',
  Ping: 'system.ping',
  Pong: 'system.pong',
  Error: 'system.error',
  GracefulStop: 'system.graceful_stop',
  RunSubmit: 'engine.run.submit',
  RunCancel: 'engine.run.cancel',
  RunResume: 'engine.run.resume',
  RunStatus: 'engine.run.status',
  EventStream: 'engine.event.stream',
  /** 计划模式：确认/否决执行计划（任务4），对应 Go 的 ipc.TypePlanConfirm。 */
  PlanConfirm: 'engine.plan.confirm',
  WorkerHealth: 'worker.health',
  WorkerStop: 'worker.stop',
  WorkerExecute: 'worker.execute',
  SecretPut: 'system.secret.put',
  SecretStatus: 'system.secret.status',
  ConfigGet: 'system.config.get',
  ConfigSet: 'system.config.set',
  ModelList: 'system.model.list'
} as const

export type FrameTypeValue = (typeof FrameType)[keyof typeof FrameType]

/** 帧编码/解码错误。 */
export class FrameError extends Error {
  constructor(message: string) {
    super(message)
    this.name = 'FrameError'
  }
}

/**
 * 把一帧编码成字节流。
 *
 * 与 Go 的 WriteFrame 保持同一语义：version 为 0 时填当前版本；
 * Type/RequestID/SessionID 长度超过 65535 视为非法。
 */
export function encodeFrame(frame: Frame): Uint8Array {
  const encoder = new TextEncoder()
  const typeBytes = encoder.encode(frame.header.type)
  const reqIdBytes = encoder.encode(frame.header.requestId)
  const sessIdBytes = encoder.encode(frame.header.sessionId)

  if (typeBytes.length > 0xffff || reqIdBytes.length > 0xffff || sessIdBytes.length > 0xffff) {
    throw new FrameError('frame header field exceeds 65535 bytes')
  }

  const payloadLength = frame.payload.length
  const version = frame.header.version === 0 ? FRAME_VERSION : frame.header.version

  const buf = new Uint8Array(
    FIXED_HEADER_SIZE + typeBytes.length + reqIdBytes.length + sessIdBytes.length + payloadLength
  )
  const view = new DataView(buf.buffer)

  buf[0] = FRAME_MAGIC_0
  buf[1] = FRAME_MAGIC_1
  buf[2] = version
  view.setUint16(3, typeBytes.length, false)
  view.setUint16(5, reqIdBytes.length, false)
  view.setUint16(7, sessIdBytes.length, false)
  // Sequence 是 uint64；JS 的 number 安全整数上限为 2^53-1，足够覆盖实际用量
  // （序号是进程内单调计数，不是哈希）。超出时 setBigUint64 会抛错，这里显式转换。
  view.setBigUint64(9, BigInt(frame.header.sequence), false)
  view.setBigInt64(17, BigInt(frame.header.deadlineMs), false)
  view.setUint32(25, payloadLength, false)

  let offset = FIXED_HEADER_SIZE
  buf.set(typeBytes, offset)
  offset += typeBytes.length
  buf.set(reqIdBytes, offset)
  offset += reqIdBytes.length
  buf.set(sessIdBytes, offset)
  offset += sessIdBytes.length
  buf.set(frame.payload, offset)

  return buf
}

/** 一帧解码所需的元信息（先读定长头，再按长度读变长部分）。 */
export interface DecodedHeader {
  header: FrameHeader
  /** 变长元信息（Type+RequestID+SessionID）的总字节数，接在定长头之后。 */
  metaLength: number
}

/**
 * 解析定长头。返回的 metaLength 告诉调用方还需要读多少字节的变长元信息。
 *
 * 分两步是因为 TCP/管道是流式的：必须知道长度才能决定还要读多少，
 * 与 Go 侧 io.ReadFull 的两次读一致。
 */
export function decodeHeader(fixed: Uint8Array): DecodedHeader {
  if (fixed.length < FIXED_HEADER_SIZE) {
    throw new FrameError(`short frame header: got ${fixed.length}, want ${FIXED_HEADER_SIZE}`)
  }
  if (fixed[0] !== FRAME_MAGIC_0 || fixed[1] !== FRAME_MAGIC_1) {
    throw new FrameError('invalid frame magic')
  }

  const version = fixed[2]
  if (version !== FRAME_VERSION) {
    throw new FrameError(`unsupported frame version: got ${version}, expected ${FRAME_VERSION}`)
  }

  const view = new DataView(fixed.buffer, fixed.byteOffset, fixed.byteLength)
  const typeLen = view.getUint16(3, false)
  const reqLen = view.getUint16(5, false)
  const sessLen = view.getUint16(7, false)
  const sequence = Number(view.getBigUint64(9, false))
  const deadlineMs = Number(view.getBigInt64(17, false))
  const payloadLength = view.getUint32(25, false)

  return {
    header: {
      version,
      // Type/RequestID/SessionID 在变长区，由调用方补齐
      type: '',
      requestId: '',
      sessionId: '',
      sequence,
      payloadLength,
      deadlineMs
    },
    metaLength: typeLen + reqLen + sessLen
  }
}

/**
 * 从"已读够一帧全部字节"的缓冲区组装出 Frame。
 *
 * @param fixed  29 字节定长头
 * @param meta   变长元信息（Type|RequestID|SessionID）
 * @param payload 载荷
 */
export function assembleFrame(
  fixed: Uint8Array,
  meta: Uint8Array,
  payload: Uint8Array
): Frame {
  const view = new DataView(fixed.buffer, fixed.byteOffset, fixed.byteLength)
  const typeLen = view.getUint16(3, false)
  const reqLen = view.getUint16(5, false)
  const sessLen = view.getUint16(7, false)

  const decoder = new TextDecoder()
  let offset = 0
  const type = decoder.decode(meta.subarray(offset, offset + typeLen))
  offset += typeLen
  const requestId = decoder.decode(meta.subarray(offset, offset + reqLen))
  offset += reqLen
  const sessionId = decoder.decode(meta.subarray(offset, offset + sessLen))

  return {
    header: {
      version: fixed[2],
      type,
      requestId,
      sessionId,
      sequence: Number(view.getBigUint64(9, false)),
      deadlineMs: Number(view.getBigInt64(17, false)),
      payloadLength: view.getUint32(25, false)
    },
    payload
  }
}

/** 判断错误是否为致命（连接已不可用），用于决定是否重连。 */
export function isFatalFrameError(err: unknown): boolean {
  if (!(err instanceof FrameError)) return true
  return !/short (frame header|frame meta|frame payload)/.test(err.message)
}

// ---------------------------------------------------------------------------
// 载荷编解码：与 internal/ipcapi/wire.go 的 Envelope 对应
// ---------------------------------------------------------------------------

/** 所有业务帧的统一载荷外壳。 */
export interface Envelope<T = unknown> {
  ok: boolean
  error?: string
  data?: T
}

export function encodeEnvelope(value: unknown): Uint8Array {
  const env: Envelope = value === undefined ? { ok: true } : { ok: true, data: value }
  return new TextEncoder().encode(JSON.stringify(env))
}

export function encodeErrorEnvelope(message: string): Uint8Array {
  return new TextEncoder().encode(JSON.stringify({ ok: false, error: message }))
}

export function decodeEnvelope<T = unknown>(payload: Uint8Array): Envelope<T> {
  if (payload.length === 0) return { ok: true }
  // 裸文本响应兼容：Go 服务端对探活/心跳应答的是原始 "PONG"/"OK" 字节，
  // 不是 JSON 信封。这里必须按成功数据处理，否则应用自己的 ping 永远失败，
  // 30 秒重连循环耗尽后表现为"后端连接失败"。
  const text = new TextDecoder().decode(payload).trim()
  if (text === 'PONG' || text === 'OK' || text === '"PONG"' || text === '"OK"') {
    return { ok: true, data: text } as Envelope<T>
  }
  try {
    const parsed = JSON.parse(text)
    if (parsed && typeof parsed === 'object' && 'ok' in parsed) {
      return parsed as Envelope<T>
    }
    return { ok: true, data: parsed } as Envelope<T>
  } catch {
    // 其余无法解析的负载按裸文本成功数据处理（与 Go 客户端的宽容语义一致）。
    return { ok: true, data: text } as Envelope<T>
  }
}

/** 解出信封里的业务数据；ok=false 时抛错。 */
export function unwrapEnvelope<T>(env: Envelope<T>): T {
  if (!env.ok) {
    throw new FrameError(env.error || 'request failed')
  }
  if (env.data === undefined) {
    return undefined as T
  }
  return env.data
}
