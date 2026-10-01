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
  /**
   * Agent 集群模式的专家数量（> 0 时开启，缺省表示关闭）。
   *
   * 引擎据此按任务内容自动组队、并行跑子代理，最后汇总成一份多视角报告。
   * 与 expert_id 互斥：集群是"我不指定谁，让系统组队"，直连是"我就要这一位"。
   */
  cluster_size?: number
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
  // 闭环报告（F2/F4）：每个终态路径恰好一次，且先于终态迁移发出。
  | 'run.closure'
  // 长期记忆召回（记忆重设计）：本次 run 点亮了哪些记忆、经由什么路径。
  | 'memory.recalled'
  // 专家子代理的工作阶段（手选专家 / Agent 集群）：主循环的 round/tool 事件说的是
  // 主 Agent 自己，子代理的 StageStarted/StageMessage/StageTool/StageFinished 走这条。
  // 载荷：{ expertId, expertName, stage, detail?, toolArgs?, result?, timestamp }。
  | 'expert.work'
  // 允许后端先于前端新增事件类型而不破坏编译。
  | (string & {})

/**
 * 闭环结论取值，对应后端 types.Closure* 常量。
 *
 * closed      —— 全部适用检查通过；
 * partial     —— 任务结束了，但有确凿的未完成：轮次预算耗尽、答案被截断、
 *                待办未做完、或某个工具最后一次调用仍失败；
 * needs_user  —— run 停在人工决定上（工具授权 / 计划确认 / 崩溃恢复裁决）；
 * failed      —— 根本没有可用答案。
 */
export type ClosureVerdict = 'closed' | 'partial' | 'needs_user' | 'failed'

/** 一条闭环检查，对应后端 types.ClosureCheck。 */
export interface ClosureCheck {
  /** 稳定机器名，如 answer_nonempty。 */
  id: string
  /** 界面显示的一行文案。 */
  label: string
  pass: boolean
  /** 失败原因（或补充说明）。 */
  note?: string
}

/** 闭环报告，对应后端 types.RunClosureReport。 */
export interface ClosureReport {
  verdict: ClosureVerdict
  checks: ClosureCheck[]
  /** verdict !== 'closed' 的便捷镜像。 */
  incomplete?: boolean
}

/** 待用户决定的工具授权请求（F5），由 tool_call.permission_required 事件写入。 */
export interface ToolPermissionRequest {
  callId: string
  toolName: string
  /** 参数摘要，用于告诉用户「将要执行什么」。 */
  args?: Record<string, unknown>
  /** 后端给出的人类可读说明。 */
  message?: string
  /** 用户已提交决定、正在等待后端接受。 */
  deciding?: boolean
  /** 提交决定失败的原因，供用户重试。 */
  decisionError?: string
}

/** 工具授权决定载荷，对应后端 ipcapi.DecidePayload。 */
export interface DecidePayload {
  run_id: string
  call_id: string
  approve: boolean
  /** 'once'（默认）| 'session'。 */
  remember?: string
}

/** 工具授权决定结果，对应后端 ipcapi.DecideResultPayload。 */
export interface DecideResultPayload {
  run_id: string
  call_id: string
  approve: boolean
  ok: boolean
}

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

  /**
   * 批准或拒绝一次待授权的工具调用（F5）。
   *
   * 与 confirmPlan 的区别：它回答的是「这一次工具调用能不能执行」，
   * 不是「这份计划行不行」。approve=false 不是错误 —— 后端会把
   * 「用户拒绝执行」作为工具结果喂回模型，让它换一条路继续。
   */
  decide(payload: DecidePayload): Promise<DecideResultPayload>

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

  // --- 记忆图（P1-c，审核文档 4.9）-----------------------------------------
  //
  // 帧名与 internal/ipc/protocol.go 的 TypeMemory* 一一对应；载荷类型是
  // internal/ipcapi/wire.go 里那组 DTO 的逐字复刻。记忆页与 run 生命周期无关，
  // 所以走 system.memory.* 命名空间，不挂在 run 上。

  /** 取一个子图（分页 / 过滤 / 邻域 / 搜索）。 */
  memoryGraph(req: MemoryGraphRequest): Promise<MemoryGraph>
  /** 取单个节点的完整详情（含完整正文、相邻边、邻居、最近召回记录）。 */
  memoryNode(id: string): Promise<MemoryNodeDetail>
  /** 改标题 / 正文 / 重要度 / 置顶 / 状态；返回更新后的详情。 */
  memoryUpdateNode(upd: MemoryNodeUpdate): Promise<MemoryNodeDetail>
  /** 硬删除节点及其所有边（「遗忘」，不可撤销）。 */
  memoryDeleteNode(id: string): Promise<void>
  /** 显式建立一条边；已存在则更新权重。 */
  memoryLink(link: MemoryLink): Promise<MemoryGraphMutation>
  /** 手动触发一次「睡眠整理」。 */
  memoryConsolidate(): Promise<MemoryGraphMutation>
  /** 导出全部记忆（可读 JSON）。 */
  memoryExport(): Promise<MemoryExport>
  /** 导入一份导出文件；按内容哈希幂等合并。 */
  memoryImport(doc: MemoryExport): Promise<MemoryGraphMutation>
  /** 计数快照与当前后端名。 */
  memoryStats(): Promise<MemoryStats>
  /**
   * 清空全部记忆（不可撤销）。
   *
   * confirm 必须是字面量 'DELETE_ALL'，与后端 ipcapi.MemoryClearPayload 一致；
   * 传其它值后端会明确拒绝，前端这里也做一次同样的校验（早失败早提示）。
   */
  memoryClear(confirm: string): Promise<MemoryGraphMutation>

  // --- 专家目录（v2.6.0）---------------------------------------------------
  //
  // 帧名与 internal/ipc/protocol.go 的 TypeExpert* 一一对应；载荷类型是
  // internal/types/t02_expert_catalog.go 里那组 DTO 的逐字复刻。专家库页此前用
  // 渲染层里的硬编码样本（60 位，而后端目录有 254 位），这一组接口把它换成
  // 后端的合并目录（内置 + 用户自定义）。

  /** 取完整专家目录（内置 + 自定义）。 */
  expertList(): Promise<ExpertListPayload>
  /** 新建或覆盖一位自定义专家；返回落库后的记录。 */
  expertSave(card: ExpertCardPayload): Promise<ExpertCardPayload>
  /** 删除一位自定义专家；内置专家删不掉（后端会明确报错）。 */
  expertDelete(id: string): Promise<ExpertDeletePayload>

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
  /**
   * MCP 服务器清单，对应后端 config 的 mcp_servers 段。
   *
   * 语义与 providers 一致：undefined 表示"未携带"，后端沿用现有配置；
   * 数组（含空数组）表示整体替换。改动需重启后端才装配生效 ——
   * MCP Worker 池的生命周期绑定在引擎启动上。
   */
  mcp_servers?: MCPServerEntryPayload[]
}

/**
 * Agent 集群模式派出的子代理数量。
 *
 * 取自后端 expert_agent 资源闸门的容量（internal/types/t02_run.go 的
 * ResourceCapacities[ResourceClassExpertAgent] = 8），与设置页「子代理模型
 * 分配」里对用户的说明「同时最多 8 个」保持一致。
 *
 * 这里刻意不给用户一个可调数字：调高没有意义 —— 超出的调用会在闸门里
 * 排队，界面看起来就像"开了没生效"。
 */
export const CLUSTER_MODE_SIZE = 8

/**
 * 一个 MCP 服务器条目，对应后端 ipcapi.MCPServerEntryPayload。
 *
 * stdio 传输用 command/args/env/cwd；http 与 sse 传输用 url/headers。
 */
export interface MCPServerEntryPayload {
  id?: string
  name?: string
  /** stdio | http | sse。留空时后端按「有 url 走 http，否则 stdio」推断。 */
  transport?: string
  /** 未设置视为启用；只有显式 false 才跳过该服务器。 */
  enabled?: boolean
  /** stdio：可执行文件与参数。 */
  command?: string
  args?: string[]
  env?: Record<string, string>
  cwd?: string
  /** http / sse：服务地址与请求头。 */
  url?: string
  headers?: Record<string, string>
  /** 非空时只暴露其中工具；denied_tools 中的工具永不暴露。 */
  allowed_tools?: string[]
  denied_tools?: string[]
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

// ---------------------------------------------------------------------------
// 记忆图（P1-c，审核文档 4.9）
// ---------------------------------------------------------------------------
//
// 这一组类型是 Go 侧 internal/types/t02_memory_graph.go 的逐字复刻：ipcapi 直接
// 复用那份 DTO 作为帧载荷（不再抄一层），所以 JSON tag 就是线上字段名。
//
// 为什么强调「逐字」：字段名抄错不会有任何编译错误，也不会在单测里暴露（前端
// 用自己构造的数据测），只会在真机上表现为「记忆页永远是空的」——最难排查的一类
// 故障。因此这里保留 Go 侧的注释语义，便于逐字段对照。

/** 记忆节点的 kind，与 mem_nodes.kind 的 CHECK 约束一致。 */
export type MemoryKind = 'fact' | 'entity' | 'episode' | 'procedure' | 'topic'

/** 记忆节点的 status。 */
export type MemoryStatus = 'active' | 'superseded' | 'archived'

/** 图的边关系，与 mem_edges.rel 的 CHECK 约束一致。 */
export type MemoryRel =
  | 'mentions'
  | 'related'
  | 'part_of'
  | 'causes'
  | 'derived_from'
  | 'supersedes'
  | 'contradicts'
  | 'same_topic'
  | 'used_with'

/** 全部 kind，供界面筛选下拉使用（顺序与 Go 侧 AllMemoryKinds 一致）。 */
export const ALL_MEMORY_KINDS: MemoryKind[] = ['fact', 'entity', 'episode', 'procedure', 'topic']

/** 全部 rel，供手动连线下拉与校验使用（顺序与 Go 侧 AllMemoryRels 一致）。 */
export const ALL_MEMORY_RELS: MemoryRel[] = [
  'mentions',
  'related',
  'part_of',
  'causes',
  'derived_from',
  'supersedes',
  'contradicts',
  'same_topic',
  'used_with'
]

/** 一次子图查询，对应 Go 的 MemoryGraphRequest。 */
export interface MemoryGraphRequest {
  /** 返回的最大节点数（0 表示由后端给一个有界默认值）。 */
  limit?: number
  /** 跳过的节点数，按 kind/importance 稳定排序，用于翻页。 */
  offset?: number
  /** 只取这些 kind；空表示全部。 */
  kinds?: MemoryKind[]
  /** 是否包含已归档节点。 */
  include_archived?: boolean
  /** 非空时只返回词法命中的节点及其直接关联。 */
  query?: string
  /** 非空时以该节点为中心取邻域。 */
  center_on?: string
  /** CenterOn 的邻域跳数（默认 1）。 */
  depth?: number
}

/** 图视图里的一个节点，对应 Go 的 MemoryGraphNode。 */
export interface MemoryGraphNode {
  id: string
  kind: string
  title?: string
  content_preview?: string
  importance: number
  pinned: boolean
  status: string
  source_run?: string
  use_count: number
  /** 节点的度；图视图据此（与 importance 一起）决定圆圈大小。 */
  degree: number
  created_at: number
  /** 0 表示从未被采用。 */
  last_used?: number
}

/**
 * 图视图里的一条有向边，对应 Go 的 MemoryGraphEdge。
 *
 * 渲染粗细必须用 effective_weight 而不是 weight：一条 45 天前用过、实际关联
 * 已经很弱的边，若按原始权重画，看起来仍然和刚建立时一样粗——那是在骗用户。
 */
export interface MemoryGraphEdge {
  src: string
  dst: string
  rel: string
  /** 原始权重。 */
  weight: number
  /** 惰性衰减后的当前强度。 */
  effective_weight: number
  fire_count?: number
}

/** 一次子图查询的结果，对应 Go 的 MemoryGraph。 */
export interface MemoryGraph {
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  /** 符合条件的节点总数（不受 limit 影响）。 */
  total: number
  /** 结果被 limit 截断。 */
  truncated?: boolean
}

/** 一条召回记录：这个节点什么时候被想起过、经由什么路径。 */
export interface MemoryRecallEntry {
  run_id: string
  via?: string
  used: boolean
  ts: number
}

/** 一个节点的完整详情，对应 Go 的 MemoryNodeDetail。 */
export interface MemoryNodeDetail {
  node: MemoryGraphNode
  /** 完整正文（列表里只有预览）。 */
  content?: string
  edges: MemoryGraphEdge[]
  neighbors: MemoryGraphNode[]
  recalls?: MemoryRecallEntry[]
}

/**
 * 改一个节点的可变字段，对应 Go 的 MemoryNodeUpdate。
 *
 * undefined 表示「不改」——刻意用可选字段而不是零值：把「用户没动这个字段」与
 * 「用户想把它置空」混为一谈，会静默清掉用户的数据。
 */
export interface MemoryNodeUpdate {
  node_id: string
  title?: string
  content?: string
  importance?: number
  pinned?: boolean
  status?: MemoryStatus
}

/** 一条待建立的边，对应 Go 的 MemoryLink。 */
export interface MemoryLink {
  src: string
  dst: string
  /** 空值表示 related。 */
  rel?: MemoryRel
  weight?: number
}

/** 写操作的结果，对应 Go 的 MemoryGraphMutation。 */
export interface MemoryGraphMutation {
  ok: boolean
  /** 受影响的节点/边数量，按操作含义解释。 */
  affected?: number
  notes?: string[]
  stats?: MemoryStats
}

/**
 * 记忆后端的计数快照，对应 Go 的 MemoryStats。
 *
 * 注意 recall_calls 等字段在 Go 侧是 uint64；正常使用量级远小于 2^53，
 * 按 number 处理不会失真。
 */
export interface MemoryStats {
  user_id?: string
  nodes: number
  edges: number
  recall_calls: number
  recall_errors: number
  recall_chars: number
  bg_dropped: number
  consolidations: number
  merged_facts: number
  archived_nodes: number
  topics_created: number
  last_error?: string
  last_error_at?: number
  /** 当前生效的后端名（synapse/embedded/mem0）。 */
  backend?: string
  enabled: boolean
}

/** 导出文件里的一个节点（含完整正文），对应 Go 的 MemoryExportNode。 */
export interface MemoryExportNode {
  id: string
  kind: string
  title?: string
  content: string
  importance: number
  pinned: boolean
  status: string
  source_run?: string
  use_count: number
  created_at: number
}

/** 一次完整导出，对应 Go 的 MemoryExport。 */
export interface MemoryExport {
  version: number
  exported_at: number
  user_id?: string
  nodes: MemoryExportNode[]
  edges: MemoryGraphEdge[]
}

/**
 * 记忆页的本地设置。
 *
 * 为什么放在前端 localStorage 而不是后端配置：总开关/自动抽取/后台整理三项最终
 * 由后端配置生效，但「嵌入模型」是可选且可能不存在的服务；这里存的是界面侧的
 * 偏好快照，随配置一起提交时以后端返回为准。
 */
export interface MemorySettings {
  /** 总开关：关闭后不再召回、不再写入。 */
  enabled: boolean
  /** 允许从对话中自动抽取事实。 */
  autoExtract: boolean
  /** 允许后台「睡眠整理」。 */
  autoConsolidate: boolean
  /** 嵌入模型名；空表示未配置。 */
  embeddingModel: string
}

/** 记忆页的默认设置。 */
export const DEFAULT_MEMORY_SETTINGS: MemorySettings = {
  enabled: true,
  autoExtract: true,
  autoConsolidate: true,
  embeddingModel: ''
}

// ---------------------------------------------------------------------------
// 专家目录（v2.6.0）
//
// 逐字对应 internal/types/t02_expert_catalog.go 的 ExpertCard / ExpertListResult /
// ExpertDeleteResult。字段只能追加：这是前后端共用的一份契约，改名字不会报错，
// 只会让界面永远显示空值。
// ---------------------------------------------------------------------------

/** 目录里一位专家，对应 Go 的 types.ExpertCard。 */
export interface ExpertCardPayload {
  id: string
  division: string
  name: string
  description: string
  /** 卡片头像字符。 */
  emoji?: string
  /** 执业风格的一句话描述。 */
  vibe?: string
  /** 人格提示词（自定义专家一般会填）。 */
  personality?: string
  /** 卡片配色（#RRGGBB 或颜色名）。 */
  color?: string
  /** 推荐工具名；真正下发给子 Agent 时会按本 build 的工具注册表过滤。 */
  tools?: string[]
  /** true 表示这条来自用户（可编辑/可删除），false 表示内置目录。 */
  custom?: boolean
}

/** 专家目录列表响应。 */
export interface ExpertListPayload {
  experts: ExpertCardPayload[]
  total: number
  /** 目录里出现的部门（去重、有序），界面据此渲染筛选条。 */
  divisions: string[]
}

/** 删除结果：deleted 为 false 表示不存在或属于内置专家。 */
export interface ExpertDeletePayload {
  deleted: boolean
}

declare global {
  interface Window {
    ximo: XimoBridge
  }
}
