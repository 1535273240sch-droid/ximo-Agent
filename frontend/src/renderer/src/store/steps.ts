/**
 * Work Log 的步骤时间线：把事件流折叠成「一次任务做了哪些内部工作」的有序列表。
 *
 * 为什么是纯函数：审核文档 2.5 要求「事件 → 步骤的映射写成一个纯函数便于单测」。
 * 这个文件不 import 任何 React、也不读时间以外的外部状态，因此可以直接对着事件
 * 数组断言输出。
 *
 * 与旧实现（tools 字典 + loose 渲染）的区别：
 *   - 步骤有顺序、有归属轮次，因此工具能落在「发生它的那一轮回答」上方，而不是
 *     全部堆在对话底部（C6）；
 *   - 复核纠偏 / 长任务续段 / 上下文压缩 / 等待授权都会产生步骤，Agent 的自愈
 *     行为对用户不再是黑盒（C7）；
 *   - 工具的成功结果与失败原因来自后端新增的 data.result / data.error（C5）。
 */

import type { ClosureReport, DurableEvent } from '@shared/types'

/** 一个步骤的种类，决定图标与分组。 */
export type StepKind = 'plan' | 'memory' | 'think' | 'tool' | 'review' | 'compact' | 'continue' | 'wait'

/** 一个步骤的状态。 */
export type StepStatus = 'running' | 'done' | 'failed' | 'waiting' | 'skipped'

/** 一个步骤的可展开细节。 */
export interface RunStepDetail {
  /** 工具入参（已脱敏）。 */
  args?: Record<string, unknown>
  /** 后端截断后的结果预览。 */
  resultPreview?: string
  /** 失败原因。 */
  error?: string
  /** 记忆召回：被点亮的节点。 */
  recalled?: { id: string; text: string; via?: string }[]
  /** 复核：结论与问题列表。 */
  verdict?: string
  issues?: string[]
}

/** 时间线上的一个步骤。 */
export interface RunStep {
  /**
   * 稳定 id：工具用 callId，其余用 `${kind}:${round}:${seq}` 之类的合成键。
   *
   * 稳定性是幂等的前提：断线重连后同一事件会被再送一次，id 相同才能 upsert
   * 成同一步而不是多出一行。
   */
  id: string
  kind: StepKind
  /** 例如 "file.read"、"第 2 轮 · 思考"。 */
  title: string
  /** 例如文件路径、命令摘要（由 args 提炼，最多 80 字）。 */
  subtitle?: string
  status: StepStatus
  round?: number
  startedAt: number
  endedAt?: number
  detail?: RunStepDetail
}

/** 等待授权的工具调用（由 tool_call.permission_required 事件写入）。 */
export interface PendingApproval {
  callId: string
  toolName: string
  args?: Record<string, unknown>
  message?: string
  /** 用户已提交决定、等待后端接受。 */
  deciding?: boolean
  /** 提交失败的原因，供重试。 */
  decisionError?: string
}

/** 一次 reduce 的产物：新的步骤数组 + 顺带学到的事实。 */
export interface StepsReduceResult {
  steps: RunStep[]
  /** 本轮事件里唯一的模型回显（若有）。 */
  modelUsed?: string
  /** 新出现的待授权调用。 */
  pendingApproval?: PendingApproval | null
}

/** 一次 run 的时间线状态。 */
export interface StepsState {
  steps: RunStep[]
  closure?: ClosureReport
  modelUsed?: string
  pendingApproval?: PendingApproval
}

/** 时间线的初始状态。 */
export const EMPTY_STEPS: StepsState = { steps: [] }

/**
 * summarizeArgs 从工具入参里提炼一行摘要。
 *
 * 只取最常见的「目标」键，并且截断到 80 字：时间线一行放不下完整 JSON，而把
 * 整个参数塞进去会让每一步都变成一堵墙。完整入参在展开态里看。
 */
export function summarizeArgs(args?: Record<string, unknown>): string | undefined {
  if (!args) return undefined
  for (const key of ['path', 'file_path', 'file', 'command', 'cmd', 'url', 'query', 'pattern', 'name']) {
    const v = args[key]
    if (typeof v === 'string' && v.trim() !== '') {
      const s = v.trim()
      return s.length > 80 ? s.slice(0, 80) + '…' : s
    }
  }
  const keys = Object.keys(args)
  if (keys.length === 0) return undefined
  const s = keys.join(', ')
  return s.length > 80 ? s.slice(0, 80) + '…' : s
}

/** upsert 把一步写进数组：已存在则合并（detail 深合并非覆盖）。 */
function upsert(steps: RunStep[], step: RunStep): RunStep[] {
  const i = steps.findIndex((x) => x.id === step.id)
  if (i < 0) return [...steps, step]
  const next = steps.slice()
  next[i] = {
    ...next[i],
    ...step,
    detail: { ...next[i].detail, ...step.detail }
  }
  return next
}

/** closeRunning 把某类仍在跑的步骤收尾。 */
function closeRunning(
  steps: RunStep[],
  kind: StepKind,
  status: StepStatus = 'done',
  at = Date.now()
): RunStep[] {
  return steps.map((x) => (x.kind === kind && x.status === 'running' ? { ...x, status, endedAt: at } : x))
}

/**
 * reduceEvent 把一条事件折叠进时间线。
 *
 * 未知事件类型一律原样返回：后端可以先行新增事件，旧前端不应因此崩掉或清空
 * 时间线（与 applyEvent 的 default 分支同一约定）。
 */
export function reduceEvent(state: StepsState, ev: DurableEvent): StepsState {
  const now = Date.now()
  const data = (ev.data ?? {}) as Record<string, unknown>
  const num = (v: unknown): number | undefined => (typeof v === 'number' ? v : undefined)
  const str = (v: unknown): string | undefined =>
    typeof v === 'string' && v !== '' ? v : undefined

  let steps = state.steps
  let modelUsed = state.modelUsed
  let pendingApproval = state.pendingApproval

  switch (ev.type) {
    case 'planning.started':
      steps = upsert(steps, {
        id: 'plan:0',
        kind: 'plan',
        title: '规划执行路径',
        status: 'running',
        startedAt: now
      })
      break

    case 'planning.completed':
      steps = upsert(steps, {
        id: 'plan:0',
        kind: 'plan',
        title: '规划执行路径',
        subtitle: typeof data.toolCount === 'number' ? `选定 ${data.toolCount} 个工具` : undefined,
        status: 'done',
        startedAt: now,
        endedAt: now
      })
      break

    case 'planning.skipped':
      steps = upsert(steps, {
        id: 'plan:0',
        kind: 'plan',
        title: '规划执行路径',
        subtitle: str(ev.message) ?? '已跳过',
        status: 'skipped',
        startedAt: now,
        endedAt: now
      })
      break

    case 'memory.recalled': {
      const items = Array.isArray(data.items) ? (data.items as { id: string; text: string; via?: string }[]) : []
      steps = upsert(steps, {
        id: `memory:${ev.seq}`,
        kind: 'memory',
        title: `回忆 ${num(data.count) ?? items.length} 条相关记忆`,
        status: 'done',
        startedAt: now,
        endedAt: now,
        detail: { recalled: items }
      })
      break
    }

    case 'round.started':
      steps = upsert(steps, {
        id: `think:${ev.round ?? 0}`,
        kind: 'think',
        title: `第 ${(ev.round ?? 0) + 1} 轮 · 思考`,
        status: 'running',
        round: ev.round,
        startedAt: now
      })
      break

    case 'round.completed': {
      if (typeof data.model === 'string' && data.model !== '') modelUsed = data.model
      steps = closeRunning(steps, 'think')
      break
    }

    case 'tool_call.requested':
      steps = upsert(steps, {
        id: ev.toolCallId ?? `tool:${ev.seq}`,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        subtitle: summarizeArgs(data.arguments as Record<string, unknown> | undefined),
        // 模型已经产出工具调用，思考步骤随之结束；否则时间线会一直转圈。
        status: 'running',
        round: ev.round,
        startedAt: now,
        detail: { args: data.arguments as Record<string, unknown> | undefined }
      })
      steps = closeRunning(steps, 'think')
      break

    case 'tool_call.started':
      steps = upsert(steps, {
        id: ev.toolCallId ?? `tool:${ev.seq}`,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        status: 'running',
        round: ev.round,
        startedAt: now
      })
      break

    case 'tool_call.completed':
      steps = upsert(steps, {
        id: ev.toolCallId ?? `tool:${ev.seq}`,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        status: 'done',
        round: ev.round,
        startedAt: now,
        endedAt: now,
        detail: {
          resultPreview: str(data.result),
          error: undefined
        }
      })
      // 需要确认的那条等待步骤就此完成。
      steps = steps.map((x) =>
        x.kind === 'wait' && x.id === `wait:${ev.toolCallId}` ? { ...x, status: 'done', endedAt: now } : x
      )
      if (pendingApproval && pendingApproval.callId === ev.toolCallId) pendingApproval = undefined
      break

    case 'tool_call.failed':
      steps = upsert(steps, {
        id: ev.toolCallId ?? `tool:${ev.seq}`,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        status: 'failed',
        round: ev.round,
        startedAt: now,
        endedAt: now,
        detail: {
          // 兼容两种来源：后端新增的 data.error，以及历史/兜底的 ev.message。
          // 这条兼容正是 C5 的修复：读错字段名会让「错误详情」永远为空。
          error: str(data.error) ?? str(ev.message),
          resultPreview: str(data.result)
        }
      })
      if (pendingApproval && pendingApproval.callId === ev.toolCallId) pendingApproval = undefined
      break

    case 'tool_call.cancelled':
      steps = upsert(steps, {
        id: ev.toolCallId ?? `tool:${ev.seq}`,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        subtitle: '用户拒绝执行',
        status: 'failed',
        round: ev.round,
        startedAt: now,
        endedAt: now,
        detail: { error: str(data.error) ?? str(ev.message) }
      })
      if (pendingApproval && pendingApproval.callId === ev.toolCallId) pendingApproval = undefined
      break

    case 'tool_call.permission_required': {
      // 不再新开一个「等待」步骤顶掉工具步骤：用户要看的是「哪个工具在等」，
      // 所以把原步骤标成 waiting，并把待授权信息挂给它（F5）。
      const callId = ev.toolCallId ?? `wait:${ev.seq}`
      steps = upsert(steps, {
        id: callId,
        kind: 'tool',
        title: ev.toolName ?? 'tool',
        subtitle: '等待授权',
        status: 'waiting',
        round: ev.round,
        startedAt: now,
        detail: { args: data.arguments as Record<string, unknown> | undefined }
      })
      pendingApproval = {
        callId,
        toolName: ev.toolName ?? '',
        args: data.arguments as Record<string, unknown> | undefined,
        message: str(ev.message)
      }
      break
    }

    case 'supervision': {
      const verdict = str(data.verdict)
      steps = upsert(steps, {
        id: `review:${ev.round ?? 0}`,
        kind: 'review',
        title: verdict && verdict !== 'on_track' ? '复核：发现偏差，已纠正' : '复核通过',
        status: 'done',
        round: ev.round,
        startedAt: now,
        endedAt: now,
        detail: {
          verdict,
          issues: Array.isArray(data.issues) ? (data.issues as string[]) : undefined
        }
      })
      break
    }

    case 'continuation':
      steps = upsert(steps, {
        id: `cont:${num(data.segment) ?? 0}`,
        kind: 'continue',
        title: `长任务续段 ${num(data.segment) ?? 0}/${num(data.maxSegments) ?? 0}`,
        status: 'done',
        startedAt: now,
        endedAt: now
      })
      break

    case 'compaction.started':
      steps = upsert(steps, {
        id: `compact:${ev.round ?? 0}`,
        kind: 'compact',
        title: '压缩上下文',
        subtitle: str(ev.message),
        status: 'running',
        round: ev.round,
        startedAt: now
      })
      break

    case 'compaction.completed':
      steps = upsert(steps, {
        id: `compact:${ev.round ?? 0}`,
        kind: 'compact',
        title: '压缩上下文',
        status: ev.err ? 'failed' : 'done',
        round: ev.round,
        startedAt: now,
        endedAt: now,
        detail: { error: ev.err?.Msg }
      })
      break

    case 'compaction.skipped':
      steps = upsert(steps, {
        id: `compact:${ev.round ?? 0}`,
        kind: 'compact',
        title: '压缩上下文',
        subtitle: str(ev.message) ?? '本期无需压缩',
        status: 'skipped',
        round: ev.round,
        startedAt: now,
        endedAt: now
      })
      break

    case 'run.closure': {
      const checks = Array.isArray(data.checks)
        ? (data.checks as { id?: unknown; label?: unknown; pass?: unknown; note?: unknown }[])
        : []
      state = {
        ...state,
        closure: {
          verdict: (str(data.verdict) ?? 'partial') as ClosureReport['verdict'],
          incomplete: data.incomplete === true,
          checks: checks.map((c) => ({
            id: String(c.id ?? ''),
            label: String(c.label ?? ''),
            pass: c.pass === true,
            note: typeof c.note === 'string' ? c.note : undefined
          }))
        }
      }
      break
    }

    // 终态：把仍在转圈的步骤收尾，避免界面残留旋转图标。
    case 'run.completed':
    case 'run.failed':
    case 'run.cancelled': {
      const status: StepStatus = ev.type === 'run.failed' ? 'failed' : 'done'
      steps = steps.map((x) => (x.status === 'running' || x.status === 'waiting' ? { ...x, status, endedAt: now } : x))
      if (ev.type !== 'run.completed') pendingApproval = undefined
      break
    }

    default:
      return state
  }

  return { steps, closure: state.closure, modelUsed, pendingApproval }
}

/**
 * reduceEvents 折叠一批事件。导出它是为了让「重连续拉」路径与实时推送路径走
 * 同一段代码，两条路径的渲染结果因此必然一致。
 */
export function reduceEvents(state: StepsState, events: DurableEvent[]): StepsState {
  let cur = state
  for (const ev of events) cur = reduceEvent(cur, ev)
  return cur
}

/** isPending 报告时间线上是否还有未结束的步骤。 */
export function isPending(steps: RunStep[]): boolean {
  return steps.some((s) => s.status === 'running' || s.status === 'waiting')
}

/**
 * currentStep 返回「当前在做什么」，用于折叠态的 ticker。
 *
 * 优先等待中的步骤（那是唯一需要用户动作的），否则最后一个进行中的步骤。
 */
export function currentStep(steps: RunStep[]): RunStep | undefined {
  for (let i = steps.length - 1; i >= 0; i--) {
    if (steps[i].status === 'waiting') return steps[i]
  }
  for (let i = steps.length - 1; i >= 0; i--) {
    if (steps[i].status === 'running') return steps[i]
  }
  return steps[steps.length - 1]
}
