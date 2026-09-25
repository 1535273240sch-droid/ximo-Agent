/**
 * 前后端共享的业务类型。
 *
 * 与 Go 侧的对应关系：
 *   - internal/ipcapi/wire.go   的 SubmitPayload / HandlePayload / RunPayload /
 *     EventsPayload / RunIDPayload / RecoveryPayload
 *   - internal/types/t02_events.go 的 Event
 *   - internal/types/t02_run.go     的 RunState
 *
 * 字段名保持 Go 的 JSON tag 原样（snake_case），这样 JSON 编解码两端零适配。
 */

/** 运行状态机取值，对应 types.RunState。 */
export type RunState =
  | 'created'
  | 'queued'
  | 'planning'
  | 'thinking'
  | 'executing'
  | 'compacting'
  | 'waiting_user'
  | 'completed'
  | 'cancelled'
  | 'failed'
  | 'recovering'

/** 终态判定，对应 types.RunState.Terminal()。 */
export function isTerminalState(state: RunState): boolean {
  return state === 'completed' || state === 'cancelled' || state === 'failed'
}

/** 可恢复判定，对应 types.RunState.Resumable()。 */
export function isResumableState(state: RunState): boolean {
  return !isTerminalState(state)
}

/** 提交任务的请求体，对应 ipcapi.SubmitPayload。 */
export interface SubmitPayload {
  session_id?: string
  prompt: string
  system_prompt?: string
  model?: string
  long_task?: boolean
  priority?: string
  max_rounds?: number
  /**
   * 开启后先出计划、等用户确认再执行（任务4）。
   * 与后端 ipcapi.SubmitPayload.PlanMode 一一对应。
   */
  plan_mode?: boolean
  /**
   * 用户在输入框旁手选的专家 ID（任务3）。非空时后端直接激活该专家编排，
   * 跳过「等主模型自己决定要不要调用 agent_expert 工具」这一步。
   * 与后端 ipcapi.SubmitPayload.ExpertID 一一对应（字段名、可选性必须一致）。
   */
  expert_id?: string
}

/** 提交后的句柄，对应 ipcapi.HandlePayload。 */
export interface HandlePayload {
  run_id: string
  session_id: string
  state: string
}

/** 运行状态查询结果，对应 ipcapi.RunPayload。 */
export interface RunPayload {
  run_id: string
  session_id: string
  state: string
  answer?: string
  round: number
  error?: string
  uncertain_tool_calls?: string[]
}

/** 一条持久化事件，对应 types.Event。 */
export interface DurableEvent {
  seq: number
  runId: string
  sessionId?: string
  type: EventType
  seqInRun?: number
  state?: RunState
  prevState?: RunState
  round?: number
  toolCallId?: string
  toolName?: string
  checkpointId?: string
  data?: Record<string, unknown>
  message?: string
  err?: { Code?: string; Msg?: string }
  coalesced?: number
  ts: string
}

/**
 * 事件类型名。
 *
 * 常见事件用联合类型穷举以便自动补全；末尾的 `(string & {})` 保留对未知
 * 类型名的兼容——后端新增事件类型时，旧前端不会因此编译失败（`applyEvent`
 * 的 default 分支本来就按"未知类型不改变状态机"处理）。
 */
export type EventType =
  | 'run.created'
  | 'run.queued'
  | 'run.started'
  | 'run.state_changed'
  | 'run.completed'
  | 'run.failed'
  | 'run.cancelled'
  | 'run.recovering'
  | 'run.resumed'
  | 'planning.started'
  | 'planning.completed'
  | 'planning.skipped'
  | 'round.started'
  | 'round.completed'
  | 'final_answer'
  | 'tool_call.requested'
  | 'tool_call.started'
  | 'tool_call.completed'
  | 'tool_call.failed'
  | 'tool_call.cancelled'
  | 'tool_call.permission_required'
  | 'error'
  | 'cancellation'
  | 'checkpoint.created'
  | 'checkpoint.loaded'
  | 'compaction.started'
  | 'compaction.completed'
  | 'compaction.skipped'
  | 'supervision'
  | 'continuation'
  | 'user_input.required'
  | 'user_input.received'
  | 'token.delta'
  | 'heartbeat'
  | 'progress'
  // 任务4新增：计划模式。
  | 'plan.proposed'
  | 'plan.confirmed'
  | 'plan.rejected'
  // 允许后端先于前端新增事件类型而不破坏编译。
  | (string & {})

/** 事件拉取结果，对应 ipcapi.EventsPayload。 */
export interface EventsPayload {
  run_id: string
  events: DurableEvent[]
  more: boolean
}

/** 恢复计划，对应 types.RecoveryPlan。 */
export interface RecoveryPlan {
  runId: string
  sessionId?: string
  decision: string
  resumeState: RunState
  lastSeq: number
  checkpointSeq: number
  round: number
  uncertainToolCalls?: string[]
  replayableToolCalls?: string[]
  sequenceOk: boolean
  notes?: string[]
}

/** 恢复结果，对应 ipcapi.RecoveryPayload。 */
export interface RecoveryPayload {
  plans: RecoveryPlan[] | null
  resumed?: string[]
  failed?: string[]
  needs_confirm?: string[]
}

/** 后端连接状态，供 UI 显示。 */
export type BackendStatus =
  | { kind: 'stopped' }
  | { kind: 'starting' }
  | { kind: 'connecting' }
  | { kind: 'ready'; pid: number; endpoint: string }
  | { kind: 'error'; message: string }

/** 从 IPCMain 暴露给渲染进程的 API 形状。 */
export interface XimoBridge {
  /** 启动后端 supervisor 进程并建立帧连接。 */
  startBackend(): Promise<BackendStatus>
  /** 停止后端进程。 */
  stopBackend(): Promise<void>
  /** 查询当前后端状态。 */
  backendStatus(): Promise<BackendStatus>

  /** 提交一个任务。 */
  submit(payload: SubmitPayload): Promise<HandlePayload>
  /** 取消一个任务。 */
  cancel(runId: string): Promise<void>
  /** 查询任务状态。 */
  status(runId: string): Promise<RunPayload>
  /** 拉取增量事件。 */
  events(runId: string, afterSeq: number): Promise<EventsPayload>
  /** 触发恢复扫描。 */
  resume(runId?: string): Promise<RecoveryPayload>
  /**
   * 确认或否决一个 run 已提出的执行计划（任务4）。
   *
   * approved 为 true 表示确认执行，false 表示要求重新规划；两者都会让后端
   * 继续推进该 run（确认则开始执行，否决则重新产出计划并再次等待）。
   */
  confirmPlan(runId: string, approved: boolean): Promise<{ run_id: string; approved: boolean; ok: boolean }>

  /** 查询 API 密钥配置状态（不含明文）。 */
  secretStatus(): Promise<SecretStatusPayload>
  /** 写入 API 密钥；返回安全存储中的引用。明文不会被回传。 */
  putSecret(value: string): Promise<{ ref: string; ok: boolean }>

  /** 读取运行时配置（不含密钥明文）。 */
  getSettings(): Promise<RuntimeSettingsPayload>
  /** 提交运行时配置变更；后端会立即生效并落盘。 */
  applySettings(payload: RuntimeSettingsPayload): Promise<{ ok: boolean }>
  /** 向服务商查询可用模型列表。base_url 非空时按该地址查询（表单当前值）。 */
  listModels(opts?: { base_url?: string }): Promise<ModelListPayload>

  /** 订阅事件推送；返回取消订阅函数。 */
  onEvent(handler: (ev: DurableEvent) => void): () => void
  /** 订阅后端状态变化。 */
  onBackendStatus(handler: (st: BackendStatus) => void): () => void

  /** 无边框窗口的自绘控制。 */
  minimizeWindow(): void
  toggleMaximizeWindow(): void
  closeWindow(): void
}

/** 密钥配置状态，对应后端 SecretStatusPayload。不含明文。 */
export interface SecretStatusPayload {
  configured: boolean
  ref: string
  /** 实际使用的安全存储后端，例如 windows-dpapi。 */
  backend: string
  /** 平台安全存储是否可用。不可用时写入会失败，不会降级为明文存储。 */
  available: boolean
}

/** 可用模型列表，对应后端 ModelListPayload。 */
export interface ModelListPayload {
  models: { id: string; owned_by?: string }[] | null
  base_url: string
  /** 非空表示查询失败（例如密钥无效、地址不对）。 */
  error?: string
}

/** 运行时配置，对应后端 RuntimeSettingsPayload。刻意不含密钥明文字段。 */
export interface RuntimeSettingsPayload {
  provider_id: string
  provider_name: string
  base_url: string
  model: string
  context_window: number
  max_output_tokens: number
  /** 密钥引用（不是密钥）。 */
  secret_ref: string
  auto_mode: string
  workspace_root: string
  db_path: string
  config_path: string
  /** 任务5新增：子代理模型池（含主服务商，顺序即优先级）。旧后端不返回该字段。 */
  providers?: ProviderEntryPayload[]
  /** 任务5新增：子代理候选模型分配。 */
  sub_agent?: SubAgentSettingsPayload
}

/** 任务5新增：候选池里的一个服务商，对应后端 ProviderEntryPayload。 */
export interface ProviderEntryPayload {
  id: string
  name: string
  base_url: string
  model: string
  /** 密钥引用（不是密钥明文）。 */
  secret_ref: string
  context_window: number
  max_output_tokens: number
  rate_limit_per_sec: number
}

/** 任务5新增：子代理候选模型分配，对应后端 SubAgentSettingsPayload。 */
export interface SubAgentSettingsPayload {
  /** 全局子代理池的候选服务商 ID 顺序。 */
  pool: string[]
  /** 专家分类 → 候选 ID 顺序。 */
  by_division: Record<string, string[]>
  /** 专家 ID → 候选 ID 顺序（优先级高于分类）。 */
  by_expert: Record<string, string[]>
}

declare global {
  interface Window {
    ximo: XimoBridge
  }
}
