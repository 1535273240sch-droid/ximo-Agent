import { useMemo } from 'react'
import { Activity, ChevronDown, ChevronRight, Loader2 } from 'lucide-react'
import { isTerminalState, type ClosureReport, type RunState } from '@shared/types'
import type { PendingApproval, RunStep } from '../../../store/steps'
import { currentStep } from '../../../store/steps'
import { WorkLogStep } from './WorkLogStep'
import { ClosureBadge } from './ClosureBadge'
import { ApprovalInline } from './ApprovalInline'
import { fmtDuration, useElapsed } from './useElapsed'

/**
 * WorkLog 把一次任务的全部「内部工作」收敛成一张可折叠的卡片（审核文档第 2 章）。
 *
 * 折叠态一行显示当前在做什么（或完成后的摘要），展开态是竖向时间线，每步可再
 * 展开看入参 / 结果 / 错误。结尾是闭环结论。
 *
 * 两个行为细节是刻意的：
 *   - 展开态由外部传入并回写（受控）。用户手动点过之后，事件更新不再覆盖它，
 *     否则用户刚点开就被下一个事件折叠回去。
 *   - 自动展开只发生在「需要用户介入」或「出问题」时（等待授权 / 失败 / 部分完成），
 *     运行正常时保持折叠，不抢占回答的位置。
 */
interface WorkLogProps {
  steps: RunStep[]
  state: RunState
  closure?: ClosureReport
  modelUsed?: string
  startedAt: number
  /** 用户手动开合后的展开态；undefined 表示用户还没动过。 */
  open?: boolean
  onToggleOpen: (open: boolean) => void
  pendingApproval?: PendingApproval
  onDecide?: (callId: string, approve: boolean) => void
  /** 闭环「继续」：以未通过检查项作为新任务继续。 */
  onContinue?: () => void
  continuing?: boolean
  /**
   * 点击「回忆到的某条记忆」→ 跳到记忆页并聚焦该节点（审核文档 4.9 第 5 条）。
   * 不传时条目退化为纯文本（旧调用点无需改动）。
   */
  onOpenMemoryNode?: (nodeId: string) => void
}

export function WorkLog({
  steps,
  state,
  closure,
  modelUsed,
  startedAt,
  open,
  onToggleOpen,
  pendingApproval,
  onDecide,
  onContinue,
  continuing,
  onOpenMemoryNode
}: WorkLogProps): React.JSX.Element | null {
  const terminal = isTerminalState(state)
  const running = !terminal
  const elapsed = useElapsed(startedAt, running)

  const current = useMemo(() => currentStep(steps), [steps])
  // 自动展开的条件（审核文档 2.2）：等待授权 / 计划确认 → 展开；失败 / 部分
  // 完成 → 展开并定位问题步骤。用户的显式选择永远优先。
  const autoOpen =
    state === 'waiting_user' || state === 'failed' || closure?.verdict === 'partial'
  const isOpen = open ?? autoOpen

  // 没有任何步骤时不渲染空卡片：一次只回复文字的任务不该多出一块空面板。
  if (steps.length === 0 && !closure) return null

  const summary = `${terminal ? '已完成' : '正在执行'} · ${steps.length} 步 · ${fmtDuration(elapsed)}`
  const stepIndex = current ? steps.indexOf(current) + 1 : steps.length

  return (
    <div className="overflow-hidden rounded-[10px] border border-border/[0.1] bg-surface/70">
      {/* 头部：折叠/展开、当前在做什么、计时 */}
      <button
        type="button"
        onClick={() => onToggleOpen(!isOpen)}
        aria-expanded={isOpen}
        aria-controls="worklog-body"
        className="flex w-full items-center gap-2 px-3 py-2 text-left transition-colors hover:bg-surface"
      >
        {isOpen ? (
          <ChevronDown size={13} className="shrink-0 text-ink-faint" />
        ) : (
          <ChevronRight size={13} className="shrink-0 text-ink-faint" />
        )}
        {running ? (
          <Loader2 size={13} className="shrink-0 animate-spin text-accent" />
        ) : (
          <Activity size={13} className="shrink-0 text-ink-faint" />
        )}

        {/* 折叠态一行：摘要，或"第 N 步 · 正在做什么"。aria-live 只播报这一行，
            逐步播报会让屏幕阅读器被工具名刷屏。 */}
        <span className="wl-ticker min-w-0 flex-1 truncate text-[12.5px] text-ink" aria-live="polite">
          <span className="block truncate">
            {terminal || !current
              ? summary
              : `第 ${stepIndex} 步 · ${current.title}${current.subtitle ? ' ' + current.subtitle : ''}`}
          </span>
        </span>

        <span className="shrink-0 font-mono text-[11.5px] text-ink-faint">{fmtDuration(elapsed)}</span>
        {closure && (
          <span
            className={[
              'shrink-0 rounded-[4px] px-1.5 py-0.5 text-[11px] font-semibold',
              closure.verdict === 'closed'
                ? 'bg-success/15 text-success'
                : closure.verdict === 'failed'
                  ? 'bg-danger/15 text-danger'
                  : 'bg-warning/15 text-warning'
            ].join(' ')}
          >
            {closure.verdict === 'closed'
              ? '✓ 已闭环'
              : closure.verdict === 'failed'
                ? '✗ 失败'
                : closure.verdict === 'needs_user'
                  ? '等待决定'
                  : '部分完成'}
          </span>
        )}
      </button>

      {/* 展开态 */}
      <div className="wl-collapse" data-open={isOpen ? 'true' : 'false'} id="worklog-body">
        <div className="wl-inner">
          {/* 次行：模型 + 轮次。模型来自后端回显，是"手选模型真的被用了"的证据。 */}
          {(modelUsed || steps.length > 0) && (
            <div className="flex items-center gap-2 border-t border-border/[0.08] px-3 py-1.5 text-[11.5px] text-ink-faint">
              {modelUsed && (
                <span className="truncate">
                  模型 <span className="font-mono text-ink-muted">{modelUsed}</span>
                </span>
              )}
              {modelUsed && <span>·</span>}
              <span>共 {steps.length} 步</span>
            </div>
          )}

          <ul className="flex flex-col px-3 py-2" role="list">
            {steps.map((s) => (
              <WorkLogStep key={s.id} step={s} onOpenMemoryNode={onOpenMemoryNode}>
                {pendingApproval && pendingApproval.callId === s.id && onDecide && (
                  <ApprovalInline
                    pending={pendingApproval}
                    onDecide={(approve) => onDecide(s.id, approve)}
                  />
                )}
              </WorkLogStep>
            ))}
          </ul>

          {closure && (
            <ClosureBadge closure={closure} onContinue={onContinue} continuing={continuing} />
          )}
        </div>
      </div>
    </div>
  )
}
