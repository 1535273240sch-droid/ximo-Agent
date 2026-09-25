import { useState } from 'react'
import { Check, ChevronDown, ChevronRight, CircleDashed, Loader2, X } from 'lucide-react'
import type { ToolTrace } from '../../store/app-store'

interface StatusView {
  label: string
  color: string
  Icon: typeof Check
  spinning?: boolean
}

const STATUS_MAP: Record<ToolTrace['status'], StatusView> = {
  requested: { label: '待调度', color: 'text-ink-faint', Icon: CircleDashed },
  started: { label: '执行中', color: 'text-accent', Icon: Loader2, spinning: true },
  completed: { label: '已完成', color: 'text-success', Icon: Check },
  failed: { label: '失败', color: 'text-danger', Icon: X }
}

interface ToolTraceListProps {
  traces: ToolTrace[]
}

export function ToolTraceList({ traces }: ToolTraceListProps): React.JSX.Element | null {
  if (traces.length === 0) return null

  return (
    <div className="flex flex-col gap-1.5 my-2">
      <div className="text-[12px] font-semibold text-ink-muted uppercase tracking-wider pl-1">
        工具执行链 ({traces.length})
      </div>
      <div className="flex flex-col gap-1.5 rounded-[8px] border border-border/[0.08] bg-surface p-2">
        {traces.map((t, idx) => (
          <ToolTraceRow key={t.callId} trace={t} isLast={idx === traces.length - 1} />
        ))}
      </div>
    </div>
  )
}

function ToolTraceRow({ trace }: { trace: ToolTrace; isLast: boolean }): React.JSX.Element {
  const [expanded, setExpanded] = useState(false)
  const view = STATUS_MAP[trace.status] ?? STATUS_MAP.requested
  const { Icon } = view

  const argsFormatted = trace.args ? JSON.stringify(trace.args, null, 2) : ''
  const hasDetails = Boolean(argsFormatted || trace.result || trace.error)

  return (
    <div className="rounded-[6px] border border-border/[0.06] bg-canvas p-2.5 transition-colors">
      <div
        onClick={() => hasDetails && setExpanded(!expanded)}
        className={[
          'flex items-center gap-2 select-none',
          hasDetails ? 'cursor-pointer' : ''
        ].join(' ')}
      >
        <div className={`flex h-5 w-5 shrink-0 items-center justify-center rounded ${view.color}`}>
          <Icon size={13} className={view.spinning ? 'animate-spin' : ''} />
        </div>

        <span className="font-mono text-[13px] font-medium text-ink">
          {trace.name}
        </span>

        <span className="text-ink-faint text-[12px]">·</span>
        <span className={`text-[12px] font-medium ${view.color}`}>{view.label}</span>

        {typeof trace.durationMs === 'number' && (
          <span className="font-mono text-[12px] text-ink-faint">
            ({trace.durationMs}ms)
          </span>
        )}

        <div className="flex-1" />

        {hasDetails && (
          <button
            type="button"
            className="text-ink-muted hover:text-ink transition-colors p-0.5"
            aria-label={expanded ? '收起详情' : '展开详情'}
          >
            {expanded ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
          </button>
        )}
      </div>

      {expanded && hasDetails && (
        <div className="mt-2.5 flex flex-col gap-2 border-t border-border/[0.08] pt-2 text-[12px]">
          {argsFormatted && (
            <div>
              <div className="font-semibold text-ink-muted mb-1">入参:</div>
              <pre className="overflow-x-auto rounded bg-surface p-2 font-mono text-ink text-[12px]">
                {argsFormatted}
              </pre>
            </div>
          )}

          {trace.result && (
            <div>
              <div className="font-semibold text-success mb-1">执行结果:</div>
              <pre className="overflow-x-auto rounded bg-surface p-2 font-mono text-ink text-[12px] max-h-[220px]">
                {trace.result}
              </pre>
            </div>
          )}

          {trace.error && (
            <div>
              <div className="font-semibold text-danger mb-1">错误详情:</div>
              <pre className="overflow-x-auto rounded bg-danger/10 p-2 font-mono text-danger text-[12px]">
                {trace.error}
              </pre>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
