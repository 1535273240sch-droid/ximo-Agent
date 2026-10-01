import { describe, expect, it } from 'vitest'
import type { DurableEvent } from '@shared/types'
import {
  currentStep,
  EMPTY_STEPS,
  isPending,
  reduceEvent,
  reduceEvents,
  summarizeArgs,
  type StepsState
} from './steps'

/**
 * 事件 → Work Log 步骤的映射必须是纯函数（审核文档 2.5），所以这里直接对着
 * 事件数组断言输出，不挂 React。
 *
 * 覆盖的是三个已确认缺陷（C5 / C6 / C7）：
 *   - C5：工具事件读错字段名，导致「执行结果」「错误详情」永远为空；
 *   - C6：工具轨迹不归位；
 *   - C7：复核 / 续段 / 压缩事件前端完全不处理。
 */

let seq = 1

/** ev 构造一条最小事件，只填测试关心到的字段。 */
function ev(type: string, data?: Record<string, unknown>, extra?: Partial<DurableEvent>): DurableEvent {
  return {
    seq: seq++,
    runId: 'run-1',
    type,
    data,
    ts: new Date().toISOString(),
    ...extra
  } as DurableEvent
}

/** reduce 从空时间线开始折叠若干事件。 */
function reduce(...events: DurableEvent[]): StepsState {
  return reduceEvents(EMPTY_STEPS, events)
}

describe('steps reducer', () => {
  it('把工具的成功结果与失败原因映射进 detail（C5）', () => {
    const s = reduce(
      ev('tool_call.requested', { arguments: { path: 'src/main.go' } }, { toolCallId: 'c1', toolName: 'file.read' }),
      ev('tool_call.completed', { success: true, result: 'package main', durationMs: 34 }, { toolCallId: 'c1', toolName: 'file.read' }),
      ev('tool_call.failed', { error: 'no such file', durationMs: 5 }, { toolCallId: 'c2', toolName: 'file.edit' })
    )

    const read = s.steps.find((x) => x.id === 'c1')
    expect(read?.status).toBe('done')
    expect(read?.subtitle).toBe('src/main.go')
    expect(read?.detail?.resultPreview).toBe('package main')
    expect(read?.detail?.args).toEqual({ path: 'src/main.go' })

    const edit = s.steps.find((x) => x.id === 'c2')
    expect(edit?.status).toBe('failed')
    // 后端新增 data.error；同时也兼容历史/兜底的 ev.message。
    expect(edit?.detail?.error).toBe('no such file')
  })

  it('在只有 message 时也能拿到错误文本（字段名兼容）', () => {
    const s = reduce(
      ev('tool_call.failed', undefined, { toolCallId: 'c9', toolName: 'file.read', message: 'legacy error text' })
    )
    expect(s.steps[0]?.detail?.error).toBe('legacy error text')
  })

  it('按发生顺序保留步骤，使工具不会全部堆到最后（C6）', () => {
    const s = reduce(
      ev('round.started', undefined, { round: 0 }),
      ev('tool_call.requested', { arguments: { path: 'a' } }, { toolCallId: 't1', toolName: 'file.read', round: 0 }),
      ev('tool_call.completed', { result: 'A' }, { toolCallId: 't1', toolName: 'file.read', round: 0 }),
      ev('round.started', undefined, { round: 1 }),
      ev('tool_call.requested', { arguments: { path: 'b' } }, { toolCallId: 't2', toolName: 'file.read', round: 1 })
    )
    expect(s.steps.map((x) => x.id)).toEqual(['think:0', 't1', 'think:1', 't2'])
    // 第 0 轮的思考步骤在模型产出工具调用时就该收尾，否则时间线一直转圈。
    expect(s.steps[0]?.status).toBe('done')
    // 工具调用已产生，第 1 轮思考也随之结束；此刻真正在跑的是 t2（未开始）。
    expect(s.steps[1]?.status).toBe('done')
    expect(s.steps[2]?.status).toBe('done')
    expect(s.steps[3]?.status).toBe('running')
  })

  it('处理复核 / 续段 / 压缩事件（C7）', () => {
    const s = reduce(
      ev('supervision', { verdict: 'off_track', issues: ['没有真正执行'] }, { round: 1 }),
      ev('continuation', { segment: 2, maxSegments: 10, completedRounds: 50 }),
      ev('compaction.started', { tier: 'compact' }, { round: 1 }),
      ev('compaction.completed', { changed: true }, { round: 1 })
    )
    expect(s.steps.map((x) => x.kind)).toEqual(['review', 'continue', 'compact'])
    expect(s.steps[0]?.title).toContain('发现偏差')
    expect(s.steps[0]?.detail?.issues).toEqual(['没有真正执行'])
    expect(s.steps[1]?.title).toContain('2/10')
    expect(s.steps[2]?.status).toBe('done')
  })

  it('等待授权把该工具步骤标成 waiting，而不是新开一步', () => {
    const s = reduce(
      ev('tool_call.requested', { arguments: { command: 'rm -rf /tmp/x' } }, { toolCallId: 'g1', toolName: 'terminal_exec' }),
      ev('tool_call.permission_required', { arguments: { command: 'rm -rf /tmp/x' } }, {
        toolCallId: 'g1',
        toolName: 'terminal_exec',
        message: '高危操作需要确认'
      })
    )
    expect(s.steps).toHaveLength(1)
    expect(s.steps[0]?.status).toBe('waiting')
    expect(s.pendingApproval?.callId).toBe('g1')
    expect(s.pendingApproval?.toolName).toBe('terminal_exec')
    // 批准后同一个调用完成，等待态必须消失。
    const after = reduceEvent(s, ev('tool_call.completed', { result: 'ok' }, { toolCallId: 'g1', toolName: 'terminal_exec' }))
    expect(after.pendingApproval).toBeUndefined()
    expect(after.steps[0]?.status).toBe('done')
  })

  it('expert.work 生成 expert 步骤：标题是专家名，结果是可展开预览', () => {
    const s = reduce(
      ev('expert.work', {
        expertId: 'engineering-frontend-developer',
        expertName: '前端开发工程师',
        stage: 'tool',
        detail: '调用工具 file_read',
        toolArgs: 'path: src/main.ts',
        result: 'export const x = 1',
        timestamp: 1_700_000_000_000
      })
    )

    expect(s.steps).toHaveLength(1)
    const step = s.steps[0]
    expect(step?.kind).toBe('expert')
    expect(step?.id).toBe('expert:engineering-frontend-developer:tool')
    expect(step?.title).toBe('前端开发工程师')
    expect(step?.subtitle).toBe('调用工具 file_read')
    expect(step?.status).toBe('done')
    expect(step?.detail?.resultPreview).toBe('export const x = 1')
    // 原始工具参数只进 detail，不进标题。
    expect(step?.title).not.toContain('src/main.ts')
  })

  it('同一位专家同一阶段的重复投递不会多出一步（重连幂等）', () => {
    const payload = {
      expertId: 'engineering-frontend-developer',
      expertName: '前端开发工程师',
      stage: 'started',
      detail: '专家开始处理任务：审查代码'
    }
    const once = reduce(ev('expert.work', payload))
    const twice = reduceEvents(once, [ev('expert.work', payload)])
    expect(twice.steps).toHaveLength(1)
    expect(twice.steps[0]?.id).toBe('expert:engineering-frontend-developer:started')
  })

  it('expert 步骤在 run.completed 后收尾，不残留进行中的行', () => {
    const s = reduce(
      ev('expert.work', { expertId: 'e1', expertName: '专家一', stage: 'started' }),
      ev('expert.work', { expertId: 'e1', expertName: '专家一', stage: 'finished' }),
      ev('run.completed', undefined, { state: 'completed' })
    )
    expect(s.steps.map((x) => x.kind)).toEqual(['expert', 'expert'])
    expect(s.steps.every((x) => x.status === 'done')).toBe(true)
    expect(isPending(s.steps)).toBe(false)
  })

  it('run.closure 写入闭环报告，终态把未结束的步骤收尾', () => {
    const s = reduce(
      ev('round.started', undefined, { round: 0 }),
      ev('run.closure', {
        verdict: 'partial',
        incomplete: true,
        checks: [
          { id: 'answer_nonempty', label: '答案非空', pass: true },
          { id: 'budget_ok', label: '未耗尽轮次预算', pass: false, note: '预算用尽' }
        ]
      }),
      ev('run.completed', undefined, { state: 'completed' })
    )
    expect(s.closure?.verdict).toBe('partial')
    expect(s.closure?.checks).toHaveLength(2)
    expect(s.steps[0]?.status).toBe('done')
    expect(isPending(s.steps)).toBe(false)
  })

  it('记录后端回显的模型（模型证据链）', () => {
    const s = reduce(ev('round.completed', { finishReason: 'stop', model: 'deepseek-chat' }, { round: 0 }))
    expect(s.modelUsed).toBe('deepseek-chat')
  })

  it('未知事件类型不改变时间线', () => {
    const before = reduce(ev('round.started', undefined, { round: 0 }))
    const after = reduceEvent(before, ev('brand.new.event', { anything: true }))
    expect(after).toBe(before)
  })

  it('同一事件重复到达是幂等的（断线重连后再送一次）', () => {
    const start = ev('tool_call.requested', { arguments: { path: 'a' } }, { toolCallId: 't1', toolName: 'file.read' })
    const done = ev('tool_call.completed', { result: 'A' }, { toolCallId: 't1', toolName: 'file.read' })
    const once = reduce(start, done)
    const twice = reduceEvents(once, [start, done])
    expect(twice.steps).toHaveLength(1)
    expect(twice.steps[0]?.status).toBe('done')
  })

  it('currentStep 优先返回等待中的步骤（那是唯一需要用户的）', () => {
    const s = reduce(
      ev('round.started', undefined, { round: 0 }),
      ev('tool_call.requested', { arguments: {} }, { toolCallId: 'g1', toolName: 'terminal_exec' }),
      ev('tool_call.permission_required', { arguments: {} }, { toolCallId: 'g1', toolName: 'terminal_exec' })
    )
    expect(currentStep(s.steps)?.id).toBe('g1')
  })

  it('summarizeArgs 提炼一行摘要并截断', () => {
    expect(summarizeArgs({ path: 'src/main.go' })).toBe('src/main.go')
    expect(summarizeArgs({ command: 'go test ./...' })).toBe('go test ./...')
    expect(summarizeArgs({})).toBeUndefined()
    const long = summarizeArgs({ path: 'x'.repeat(200) })
    expect(long?.length).toBe(81) // 80 字 + 省略号
  })
})
