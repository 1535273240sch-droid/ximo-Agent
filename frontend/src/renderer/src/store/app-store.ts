/**
 * 全局状态。
 *
 * 分层：
 *   - connection:  后端进程 + 帧通道的可用性
 *   - sessions:    会话列表（本地持久化）
 *   - runs:        每个会话当前 run 的状态与消息（由事件流驱动）
 *   - ui:          主题、侧栏开合等纯界面状态
 *
 * 关键设计：run 状态一律由**事件流**推导，而不是靠轮询 status 接口。
 * 这与后端的事件溯源模型一致（第9章），也天然支持断线重连后的状态重建
 * （第26章）——重连时只要以 lastSeq 续拉事件即可，不需要重新问一遍"现在什么状态"。
 */

import { create } from 'zustand'
import type {
  BackendStatus,
  DurableEvent,
  RunState,
  SubmitPayload
} from '@shared/types'
import { isTerminalState } from '@shared/types'
import type { Expert } from '../components/experts/experts-data'

/** GO 模式拼进 system_prompt 的固定文案（任务1约定，纯前端拼接）。 */
const GO_MODE_SYSTEM_PROMPT =
  '本次任务请直接执行，不要在低风险步骤上反复征求确认，完成后直接给出结果。'

/** GO 模式的 system_prompt 拼接规则：在用户原有提示词基础上追加固定文案。 */
const buildGoSystemPrompt = (base?: string): string =>
  base && base.trim() ? `${base.trim()}\n${GO_MODE_SYSTEM_PROMPT}` : GO_MODE_SYSTEM_PROMPT
import { DEFAULT_THEME, applyTheme, type ThemeId } from '../themes/tokens'

/** 一条对话消息（由事件流累积而成）。 */
export interface ChatMessage {
  id: string
  role: 'user' | 'assistant' | 'system'
  content: string
  reasoning?: string
  /** 该消息关联的工具调用，用于渲染执行轨迹。 */
  toolCalls?: ToolTrace[]
  createdAt: number
  /** 流式输出中：UI 用光标/微光提示，尚未定型。 */
  streaming?: boolean
}

/** 一次工具调用的可视化轨迹。 */
export interface ToolTrace {
  callId: string
  name: string
  status: 'requested' | 'started' | 'completed' | 'failed'
  args?: Record<string, unknown>
  result?: string
  error?: string
  durationMs?: number
}

/** 一个会话。 */
export interface Session {
  id: string
  title: string
  createdAt: number
  updatedAt: number
  /** 该会话当前的活跃 run（若有）。 */
  activeRunId?: string
}

/** 一个 run 的界面态。 */
export interface RunView {
  runId: string
  sessionId: string
  state: RunState
  round: number
  answer: string
  error?: string
  /** 已吸收的最大事件序号，重连续拉与去重用。 */
  lastSeq: number
  /** 事件驱动的消息列表。 */
  messages: ChatMessage[]
  /** 尚未匹配到消息的工具调用轨迹。 */
  tools: Record<string, ToolTrace>
  startedAt: number
  updatedAt: number
  /**
   * 待用户决定的执行计划（任务4）。
   *
   * 由 plan.proposed 事件写入，由 plan.confirmed / plan.rejected 清除。
   * run.state 为 waiting_user 且本字段存在时，界面展示计划确认卡片。
   */
  pendingPlan?: PlanProposal
}

/** 一份等待用户确认的执行计划（任务4）。 */
export interface PlanProposal {
  /** 计划全文（Markdown）。 */
  plan: string
  /** 解析出的步骤列表；模型输出不规整时可能为空，界面回退到渲染全文。 */
  steps: string[]
  /** 第几版计划，从 1 开始；重新规划后递增。 */
  revision: number
  /** 用户已提交决定、正在等待后端接受。 */
  deciding?: boolean
  /** 提交决定失败的原因，供用户重试。 */
  decisionError?: string
}

interface StoreState {
  // --- 连接 ---
  backend: BackendStatus
  setBackend: (st: BackendStatus) => void

  // --- 主题 ---
  theme: ThemeId
  setTheme: (t: ThemeId) => void

  // --- 会话 ---
  sessions: Session[]
  activeSessionId: string | null
  createSession: () => string
  selectSession: (id: string) => void
  renameSession: (id: string, title: string) => void
  deleteSession: (id: string) => void

  // --- 输入框选项（任务1/3/4，跨组件共享） ---
  // 放在 store 而不是 Composer 本地 state：空会话的「建议探索场景」按钮也走
  // submit，若这些开关留在 Composer 局部，场景提交会绕过计划模式等开关。
  /** 计划模式开关（任务4）。 */
  planMode: boolean
  setPlanMode: (v: boolean) => void
  /** GO 模式开关（任务1）。 */
  goMode: boolean
  setGoMode: (v: boolean) => void
  /** 本次选用的模型（任务1）；undefined 表示跟随设置页全局默认。 */
  model?: string
  setModel: (m?: string) => void
  /** 手选绑定的专家（任务3）；随本次提交生效后自动清空。 */
  selectedExpert?: Expert
  setSelectedExpert: (e?: Expert) => void

  // --- runs ---
  runs: Record<string, RunView>
  submit: (prompt: string, opts?: Partial<SubmitPayload>) => Promise<void>
  cancelActive: () => Promise<void>
  /** 确认或否决某 run 的计划（任务4）。 */
  confirmPlan: (runId: string, approved: boolean) => Promise<void>
  applyEvent: (ev: DurableEvent) => void
  setRunFromStatus: (payload: {
    run_id: string
    session_id: string
    state: string
    answer?: string
    round: number
    error?: string
  }) => void

  // --- ui ---
  sidebarOpen: boolean
  toggleSidebar: () => void
  /** 当前主视图。 */
  view: 'chat' | 'experts' | 'knowledge' | 'settings'
  setView: (v: StoreState['view']) => void
}

const SESSIONS_KEY = 'ximo.sessions.v2'
const THEME_KEY = 'ximo.theme.v2'

/** 从 localStorage 读取会话列表。 */
function loadSessions(): Session[] {
  try {
    const raw = localStorage.getItem(SESSIONS_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw) as Session[]
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function saveSessions(sessions: Session[]): void {
  try {
    localStorage.setItem(SESSIONS_KEY, JSON.stringify(sessions))
  } catch {
    // 存储配额满或被禁用：会话列表丢失不影响当前会话可用。
  }
}

function loadTheme(): ThemeId {
  try {
    const raw = localStorage.getItem(THEME_KEY)
    if (
      raw === 'obsidian' ||
      raw === 'graphite' ||
      raw === 'silver' ||
      raw === 'warm-white' ||
      raw === 'paper'
    ) {
      return raw
    }
  } catch {
    // 忽略
  }
  return DEFAULT_THEME
}

function newId(prefix: string): string {
  const rand = Math.random().toString(36).slice(2, 10)
  return `${prefix}-${Date.now().toString(36)}-${rand}`
}

/** 从用户的 prompt 生成一个简短的会话标题。 */
function deriveTitle(prompt: string): string {
  const clean = prompt.replace(/\s+/g, ' ').trim()
  if (clean.length <= 24) return clean || '新会话'
  return clean.slice(0, 24) + '…'
}

/**
 * 流式诊断日志开关（任务2 Part A）：默认关闭，在控制台执行
 * localStorage.setItem('ximo.debug.stream', '1') 后刷新页面开启，与
 * provider 端 XIMO_STREAM_DEBUG=1 的日志对比到达间隔分布。
 * token.delta 是约 50ms 一帧的热路径，开关只在首次用到时读一次 localStorage。
 */
let streamDebugEnabled: boolean | null = null
function isStreamDebugEnabled(): boolean {
  if (streamDebugEnabled === null) {
    try {
      streamDebugEnabled = localStorage.getItem('ximo.debug.stream') === '1'
    } catch {
      // localStorage 不可用（如存储被禁用）：诊断日志保持关闭即可。
      streamDebugEnabled = false
    }
  }
  return streamDebugEnabled
}

export const useStore = create<StoreState>((set, get) => ({
  // ---------------------------------------------------------------------
  // 连接
  // ---------------------------------------------------------------------
  backend: { kind: 'stopped' },
  setBackend: (st) => set({ backend: st }),

  // ---------------------------------------------------------------------
  // 主题
  // ---------------------------------------------------------------------
  theme: loadTheme(),
  setTheme: (t) => {
    applyTheme(t)
    try {
      localStorage.setItem(THEME_KEY, t)
    } catch {
      // 忽略
    }
    set({ theme: t })
  },

  // ---------------------------------------------------------------------
  // 会话
  // ---------------------------------------------------------------------
  sessions: loadSessions(),
  activeSessionId: null,

  createSession: () => {
    const id = newId('sess')
    const now = Date.now()
    const session: Session = { id, title: '新会话', createdAt: now, updatedAt: now }
    const sessions = [session, ...get().sessions]
    saveSessions(sessions)
    set({ sessions, activeSessionId: id, view: 'chat' })
    return id
  },

  selectSession: (id) => set({ activeSessionId: id, view: 'chat' }),

  renameSession: (id, title) => {
    const sessions = get().sessions.map((s) =>
      s.id === id ? { ...s, title, updatedAt: Date.now() } : s
    )
    saveSessions(sessions)
    set({ sessions })
  },

  deleteSession: (id) => {
    const sessions = get().sessions.filter((s) => s.id !== id)
    saveSessions(sessions)
    const nextActive =
      get().activeSessionId === id ? (sessions[0]?.id ?? null) : get().activeSessionId
    set({ sessions, activeSessionId: nextActive })
  },

  // ---------------------------------------------------------------------
  // 输入框选项（任务1/3/4）
  // ---------------------------------------------------------------------
  planMode: false,
  setPlanMode: (v) => set({ planMode: v }),
  goMode: false,
  setGoMode: (v) => set({ goMode: v }),
  model: undefined,
  setModel: (m) => set({ model: m }),
  selectedExpert: undefined,
  setSelectedExpert: (e) => set({ selectedExpert: e }),

  // ---------------------------------------------------------------------
  // runs
  // ---------------------------------------------------------------------
  runs: {},

  submit: async (prompt, opts) => {
    const trimmed = prompt.trim()
    if (!trimmed) return

    // 输入框选项统一在这里组装（此前在 Composer 内）：空会话的「建议探索
    // 场景」按钮同样走 submit —— 若不在此处合并，场景提交会绕过计划模式等
    // 开关（界面表现为"开了计划却不出确认卡片"）。
    // 规则：plan_mode 与 expert_id 互斥 —— 专家直连路径自带方案阶段、会忽略
    // 计划模式（后端有测试固化的组合规则）。
    let effective = opts
    if (!effective) {
      const st = get()
      const built: Partial<SubmitPayload> = {}
      if (st.model) built.model = st.model
      if (st.goMode) built.system_prompt = buildGoSystemPrompt()
      if (st.planMode && !st.selectedExpert) built.plan_mode = true
      if (st.selectedExpert) built.expert_id = st.selectedExpert.id
      effective = Object.keys(built).length > 0 ? built : undefined
      // 专家绑定是「本次消息」语义：提交后即清空。
      if (st.selectedExpert) set({ selectedExpert: undefined })
    }

    // 没有活跃会话时自动开一个。
    let sessionId = get().activeSessionId
    if (!sessionId) {
      sessionId = get().createSession()
    }

    // 先乐观地把用户消息放进界面，让输入立刻有反馈。
    const pendingRunId = newId('pending')
    const userMsg: ChatMessage = {
      id: newId('msg'),
      role: 'user',
      content: trimmed,
      createdAt: Date.now()
    }

    set((st) => ({
      runs: {
        ...st.runs,
        [pendingRunId]: {
          runId: pendingRunId,
          sessionId: sessionId as string,
          state: 'queued',
          round: 0,
          answer: '',
          lastSeq: 0,
          messages: [userMsg],
          tools: {},
          startedAt: Date.now(),
          updatedAt: Date.now()
        }
      },
      sessions: st.sessions.map((s) =>
        s.id === sessionId
          ? {
              ...s,
              title: s.title === '新会话' ? deriveTitle(trimmed) : s.title,
              updatedAt: Date.now(),
              activeRunId: pendingRunId
            }
          : s
      )
    }))
    saveSessions(get().sessions)

    try {
      const handle = await window.ximo.submit({
        ...effective,
        session_id: sessionId,
        prompt: trimmed
      })

      // 真实 runID 到手后，把临时记录迁移到真实 runID 下。
      set((st) => {
        const view = st.runs[pendingRunId]
        const runs = { ...st.runs }
        delete runs[pendingRunId]
        if (view) {
          runs[handle.run_id] = {
            ...view,
            runId: handle.run_id,
            state: (handle.state as RunState) || 'queued'
          }
        }
        return {
          runs,
          sessions: st.sessions.map((s) =>
            s.id === sessionId ? { ...s, activeRunId: handle.run_id } : s
          )
        }
      })
      saveSessions(get().sessions)
    } catch (err) {
      // 提交失败：把错误落到界面上，而不是静默丢弃。
      set((st) => {
        const view = st.runs[pendingRunId]
        if (!view) return st
        return {
          runs: {
            ...st.runs,
            [pendingRunId]: {
              ...view,
              state: 'failed',
              error: (err as Error).message
            }
          }
        }
      })
    }
  },

  cancelActive: async () => {
    const { activeSessionId, sessions, runs } = get()
    const session = sessions.find((s) => s.id === activeSessionId)
    if (!session?.activeRunId) return
    const view = runs[session.activeRunId]
    if (!view || isTerminalState(view.state)) return
    try {
      await window.ximo.cancel(session.activeRunId)
    } catch (err) {
      // 取消失败（例如 run 已经结束）：把原因记到 run 上供界面显示。
      set((st) => ({
        runs: {
          ...st.runs,
          [view.runId]: { ...view, error: (err as Error).message }
        }
      }))
    }
  },

  /**
   * 确认或否决某 run 的计划（任务4）。
   *
   * 只是把用户的点击转给后端：真正的状态变更由后端继续推进该 run 后发出的
   * 事件（plan.confirmed / plan.rejected）驱动，与"run 状态一律由事件流推导"
   * 的既有约定一致。这里只在提交期间把按钮置为等待态，失败时把原因留在卡片上
   * 供用户重试。
   */
  confirmPlan: async (runId, approved) => {
    const view = get().runs[runId]
    if (!view?.pendingPlan || view.pendingPlan.deciding) return

    set((st) => {
      const cur = st.runs[runId]
      if (!cur?.pendingPlan) return st
      return {
        runs: {
          ...st.runs,
          [runId]: {
            ...cur,
            pendingPlan: { ...cur.pendingPlan, deciding: true, decisionError: undefined }
          }
        }
      }
    })

    try {
      await window.ximo.confirmPlan(runId, approved)
      // 成功后不改本地状态：卡片交给随后的 plan.confirmed / plan.rejected 事件
      // 关闭，避免"界面已关闭但后端还在等待"的假象。
    } catch (err) {
      set((st) => {
        const cur = st.runs[runId]
        if (!cur?.pendingPlan) return st
        return {
          runs: {
            ...st.runs,
            [runId]: {
              ...cur,
              pendingPlan: {
                ...cur.pendingPlan,
                deciding: false,
                decisionError: (err as Error).message
              }
            }
          }
        }
      })
    }
  },

  /**
   * 吸收一条事件并更新对应的 run。
   *
   * 幂等性：事件按 seq 单调递增到达，重复的（seq <= lastSeq）直接丢弃。
   * 这保证断线重连后"续拉 + 实时推送"两条路径同时有数据时不会重复渲染。
   */
  applyEvent: (ev) => {
    set((st) => {
      const view = st.runs[ev.runId]
      if (!view) return st
      // 合并后的临时帧（coalescer 产出的 token.delta 等）没有持久化序号，以
      // seq=0 表示"始终投递"（engine.streamRun 同款约定）。它们不参与去重、
      // 也不推进 lastSeq——否则 0 <= lastSeq 恒真，流式增量会被整体丢弃，
      // 界面只能等 final_answer 一次性出全文（"一瞬间蹦出一堆"的根源）。
      if (ev.seq > 0 && ev.seq <= view.lastSeq) return st

      const next: RunView = {
        ...view,
        lastSeq: ev.seq > 0 ? ev.seq : view.lastSeq,
        updatedAt: Date.now(),
        messages: [...view.messages],
        tools: { ...view.tools }
      }

      const data = ev.data ?? {}

      switch (ev.type) {
        case 'run.queued':
        case 'run.state_changed':
        case 'run.recovering':
        case 'run.resumed':
          if (ev.state) next.state = ev.state
          break

        case 'planning.started':
        case 'planning.completed':
          next.state = 'planning'
          break

        case 'round.started':
          next.state = 'thinking'
          if (typeof ev.round === 'number') next.round = ev.round
          break

        case 'round.completed':
          if (typeof ev.round === 'number') next.round = ev.round
          break

        case 'token.delta': {
          // 流式增量：累积到最后一条 assistant 消息上；没有就新建一条。
          const delta = String(data.content ?? data.text ?? '')
          const reasoning = String(data.reasoning ?? '')
          // 任务2 Part A 诊断日志：开关定义见模块级 isStreamDebugEnabled
          // （控制台执行 localStorage.setItem('ximo.debug.stream', '1') 后刷新
          // 开启），与 provider 端 XIMO_STREAM_DEBUG=1 的日志对比到达间隔分布。
          // 热路径不逐条读 localStorage，开关只在首次用到时读取一次。
          if (isStreamDebugEnabled()) {
            console.debug(
              `[stream-debug] ${new Date().toISOString()} token.delta len=${delta.length} reasoning=${reasoning.length} seq=${ev.seq} coalesced=${ev.coalesced ?? 1}`
            )
          }
          const last = next.messages[next.messages.length - 1]
          if (last && last.role === 'assistant' && last.streaming) {
            next.messages[next.messages.length - 1] = {
              ...last,
              content: last.content + delta,
              reasoning: reasoning ? (last.reasoning ?? '') + reasoning : last.reasoning
            }
          } else if (delta || reasoning) {
            next.messages.push({
              id: newId('msg'),
              role: 'assistant',
              content: delta,
              reasoning: reasoning || undefined,
              createdAt: Date.now(),
              streaming: true
            })
          }
          break
        }

        case 'tool_call.requested': {
          if (ev.toolCallId) {
            next.tools[ev.toolCallId] = {
              callId: ev.toolCallId,
              name: ev.toolName ?? 'unknown',
              status: 'requested',
              args: (data.arguments as Record<string, unknown>) ?? undefined
            }
          }
          break
        }

        case 'tool_call.started': {
          if (ev.toolCallId) {
            const prev = next.tools[ev.toolCallId]
            next.tools[ev.toolCallId] = {
              callId: ev.toolCallId,
              name: ev.toolName ?? prev?.name ?? 'unknown',
              status: 'started',
              args: prev?.args
            }
          }
          break
        }

        case 'tool_call.completed':
        case 'tool_call.failed': {
          if (ev.toolCallId) {
            const prev = next.tools[ev.toolCallId]
            next.tools[ev.toolCallId] = {
              callId: ev.toolCallId,
              name: ev.toolName ?? prev?.name ?? 'unknown',
              status: ev.type === 'tool_call.completed' ? 'completed' : 'failed',
              args: prev?.args,
              result: data.result ? String(data.result) : undefined,
              error: data.error ? String(data.error) : undefined,
              durationMs:
                typeof data.durationMs === 'number' ? (data.durationMs as number) : undefined
            }
          }
          break
        }

        case 'final_answer': {
          // 定稿：把流式消息定型，或补一条完整答案。
          const answer = String(data.answer ?? ev.message ?? '')
          const last = next.messages[next.messages.length - 1]
          if (last && last.role === 'assistant' && last.streaming) {
            next.messages[next.messages.length - 1] = {
              ...last,
              content: answer || last.content,
              streaming: false
            }
          } else if (
            answer &&
            // 兜底对账可能已经把同内容的定稿消息落进列表，此处去重。
            !(
              last &&
              last.role === 'assistant' &&
              !last.streaming &&
              last.content === answer
            )
          ) {
            next.messages.push({
              id: newId('msg'),
              role: 'assistant',
              content: answer,
              createdAt: Date.now()
            })
          }
          next.answer = answer
          break
        }

        case 'error': {
          // 引擎发出的错误事件携带真实失败原因（如 "HTTP 400: unknown
          // provider for model ..."）。落到 run.error 供界面横幅显示；
          // 若随后 run 正常完成（非致命错误），completed 分支会将其清除。
          const msg = String(ev.message ?? data.error ?? data.reason ?? '')
          if (msg) next.error = msg
          break
        }

        case 'run.completed':
          next.state = 'completed'
          next.error = undefined
          next.messages = next.messages.map((m) =>
            m.streaming ? { ...m, streaming: false } : m
          )
          break

        case 'run.failed':
          next.state = 'failed'
          next.error = String(data.error ?? data.reason ?? ev.message ?? 'run failed')
          next.messages = next.messages.map((m) =>
            m.streaming ? { ...m, streaming: false } : m
          )
          break

        case 'run.cancelled':
          next.state = 'cancelled'
          next.messages = next.messages.map((m) =>
            m.streaming ? { ...m, streaming: false } : m
          )
          break

        case 'cancellation':
          if (ev.state) next.state = ev.state
          break

        // 任务4新增：计划模式。
        case 'plan.proposed': {
          // 后端产出了待确认的计划，run 已停在 waiting_user 等用户决定。
          const plan = String(data.plan ?? ev.message ?? '')
          const rawSteps = Array.isArray(data.steps) ? data.steps : []
          next.pendingPlan = {
            plan,
            steps: rawSteps.map((s) => String(s)).filter((s) => s !== ''),
            revision: typeof data.revision === 'number' ? (data.revision as number) : 1
          }
          next.state = 'waiting_user'
          break
        }

        // 任务4新增：用户确认计划，去掉卡片，进入执行。
        case 'plan.confirmed':
          next.pendingPlan = undefined
          break

        // 任务4新增：用户否决计划，去掉（或更新为）待确认计划。
        case 'plan.rejected':
          next.pendingPlan = undefined
          break

        default:
          // 未知事件类型不改变状态机，但 lastSeq 已经推进，不会重复处理。
          break
      }

      return { runs: { ...st.runs, [ev.runId]: next } }
    })
  },

  setRunFromStatus: (p) => {
    set((st) => {
      const view = st.runs[p.run_id]
      if (!view) return st
      const next: RunView = {
        ...view,
        state: p.state as RunState,
        answer: p.answer ?? view.answer,
        round: p.round,
        error: p.error ?? view.error,
        updatedAt: Date.now()
      }
      // 兜底对账的对偶：事件路径全部失效时（例如订阅被背压断开），
      // 终态状态里携带的 answer 是回复唯一的来源。这里把没有落成消息的
      // answer 补一条最终消息，保证"回复不显示"在任何链路异常下都不会发生。
      if (next.state === 'completed' && next.answer && next.answer.trim() !== '') {
        const hasAssistantContent = next.messages.some(
          (m) => m.role === 'assistant' && m.content.trim() !== ''
        )
        next.messages = next.messages.map((m) => (m.streaming ? { ...m, streaming: false } : m))
        if (!hasAssistantContent) {
          next.messages = [
            ...next.messages,
            {
              id: newId('msg'),
              role: 'assistant',
              content: next.answer,
              createdAt: Date.now()
            }
          ]
        }
      } else {
        next.messages = next.messages.map((m) => (m.streaming ? { ...m, streaming: false } : m))
      }
      return {
        runs: {
          ...st.runs,
          [p.run_id]: next
        }
      }
    })
  },

  // ---------------------------------------------------------------------
  // ui
  // ---------------------------------------------------------------------
  sidebarOpen: true,
  toggleSidebar: () => set((st) => ({ sidebarOpen: !st.sidebarOpen })),

  view: 'chat',
  setView: (v) => set({ view: v })
}))

/** 取某会话的 run 视图（含临时的 pending run）。 */
export function selectRunForSession(
  runs: Record<string, RunView>,
  sessionId: string | null
): RunView | null {
  if (!sessionId) return null
  const candidates = Object.values(runs).filter((r) => r.sessionId === sessionId)
  if (candidates.length === 0) return null
  return candidates.sort((a, b) => b.updatedAt - a.updatedAt)[0]
}
