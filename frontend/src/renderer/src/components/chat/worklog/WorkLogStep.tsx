import { useState } from 'react'
import { ChevronDown, ChevronRight, ShieldQuestion, Wrench, X } from 'lucide-react'
import type { RunStep } from '../../../store/steps'
import { fmtDuration } from './useElapsed'

/**
 * 单条步骤的状态圆点。
 *
 * 语义（审核文档 2.3）：进行中 = accent、完成 = success、失败 = danger、
 * 等待 = warning。图标按种类挑，颜色只表达状态，两者不混用，这样扫一眼就能
 * 分辨「哪一步出事了」而不是「哪一步是工具」。
 */
export function StepDot({ step }: { step: RunStep }): React.JSX.Element {
  const status = step.status
  const color =
    status === 'failed'
      ? 'text-danger'
      : status === 'waiting'
        ? 'text-warning'
        : status === 'running'
          ? 'text-accent'
          : status === 'skipped'
            ? 'text-ink-faint'
            : 'text-success'

  const label =
    step.kind === 'plan'
      ? '规划'
      : step.kind === 'memory'
        ? '记忆'
        : step.kind === 'think'
          ? '思考'
          : step.kind === 'tool'
            ? '工具'
            : step.kind === 'review'
              ? '复核'
              : step.kind === 'compact'
                ? '压缩'
                : step.kind === 'continue'
                  ? '续段'
                  : '等待'

  return (
    <span
      className={`wl-dot ${color}`}
      data-status={status}
      role="img"
      aria-label={`${label}：${statusLabel(status)}`}
      title={`${label}：${statusLabel(status)}`}
    >
      {status === 'running' ? (
        <span className="block h-2.5 w-2.5 rounded-full bg-current" />
      ) : status === 'failed' ? (
        <X size={11} strokeWidth={3} className="block" />
      ) : status === 'waiting' ? (
        <ShieldQuestion size={11} strokeWidth={2.6} className="block" />
      ) : status === 'skipped' ? (
        <span className="block h-2.5 w-2.5 rounded-full border border-current" />
      ) : (
        <span className="block h-2 w-2 rounded-full bg-current" />
      )}
    </span>
  )
}

/** statusLabel 把状态渲染成中文，用于无障碍朗读与 tooltip。 */
export function statusLabel(status: RunStep['status']): string {
  switch (status) {
    case 'running':
      return '进行中'
    case 'waiting':
      return '等待你的决定'
    case 'failed':
      return '失败'
    case 'skipped':
      return '已跳过'
    default:
      return '完成'
  }
}

/**
 * 一条时间线步骤。
 *
 * 展开态只显示「值得展开」的东西（入参、结果、错误、记忆条目、复核问题），
 * 没有内容的步骤不显示展开箭头——一个点了什么都不发生的箭头比没有箭头更糟。
 */
export function WorkLogStep({
  step,
  children,
  onOpenMemoryNode
}: {
  step: RunStep
  children?: React.ReactNode
  /** 点击「回忆到的某条记忆」→ 跳到记忆页并聚焦该节点（审核文档 4.9 第 5 条）。 */
  onOpenMemoryNode?: (nodeId: string) => void
}): React.JSX.Element {
  const [open, setOpen] = useState(false)
  const d = step.detail
  const hasDetail = Boolean(
    d &&
      (d.args ||
        d.resultPreview ||
        d.error ||
        (d.recalled && d.recalled.length > 0) ||
        d.verdict ||
        (d.issues && d.issues.length > 0))
  )
  const durationMs =
    step.endedAt !== undefined && step.startedAt ? step.endedAt - step.startedAt : undefined

  return (
    <li className="wl-step relative flex gap-2.5 pl-1" data-status={step.status}>
      {/* 竖向连线：进行中的那一段用流动渐变（CSS 里有定义）。 */}
      <span className="absolute left-[10px] top-4 h-[calc(100%-6px)] w-px bg-border/[0.12]" aria-hidden />
      <span
        className="wl-line absolute left-[10px] top-4 h-[calc(100%-6px)] w-px"
        data-active={step.status === 'running' ? 'true' : 'false'}
        aria-hidden
      />
      <span className="relative z-[1] flex h-[21px] w-[21px] shrink-0 items-center justify-center rounded-full bg-canvas pt-[1px]">
        <StepDot step={step} />
      </span>

      <div className="min-w-0 flex-1 pb-2">
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => hasDetail && setOpen((v) => !v)}
            disabled={!hasDetail}
            className={[
              'flex min-w-0 flex-1 items-center gap-1.5 text-left text-[12.5px]',
              hasDetail ? 'cursor-pointer hover:text-ink' : 'cursor-default'
            ].join(' ')}
            aria-expanded={hasDetail ? open : undefined}
          >
            {hasDetail ? (
              open ? (
                <ChevronDown size={12} className="shrink-0 text-ink-faint" />
              ) : (
                <ChevronRight size={12} className="shrink-0 text-ink-faint" />
              )
            ) : (
              <span className="w-3 shrink-0" />
            )}
            <Wrench
              size={11}
              className={`shrink-0 ${step.kind === 'tool' ? 'text-ink-faint' : 'hidden'}`}
            />
            <span
              className={[
                'truncate font-mono',
                step.status === 'failed' ? 'text-danger' : 'text-ink'
              ].join(' ')}
            >
              {step.title}
            </span>
            {step.subtitle && (
              <span className="truncate text-[12px] text-ink-faint">{step.subtitle}</span>
            )}
          </button>
          {durationMs !== undefined && durationMs > 0 && (
            <span className="shrink-0 font-mono text-[11.5px] text-ink-faint">
              {fmtDuration(durationMs)}
            </span>
          )}
          {step.status === 'failed' && (
            <span className="shrink-0 text-[11.5px] font-medium text-danger">失败</span>
          )}
        </div>

        {/* 步骤内联内容（批准 / 拒绝按钮等）。 */}
        {children}

        {open && hasDetail && d && (
          <div className="mt-1.5 rounded-[6px] border border-border/[0.08] bg-surface/60 p-2">
            <StepDetailBody detail={d} onOpenMemoryNode={onOpenMemoryNode} />
          </div>
        )}
      </div>
    </li>
  )
}

/** StepDetailBody 渲染一个步骤的可展开细节。 */
function StepDetailBody({
  detail,
  onOpenMemoryNode
}: {
  detail: NonNullable<RunStep['detail']>
  onOpenMemoryNode?: (nodeId: string) => void
}): React.JSX.Element {
  return (
    <div className="flex flex-col gap-2">
      {detail.args && Object.keys(detail.args).length > 0 && (
        <DetailBlock label="入参">
          <pre className="max-h-[180px] overflow-auto whitespace-pre-wrap break-words font-mono text-[11.5px] leading-relaxed text-ink-muted">
            {JSON.stringify(detail.args, null, 2)}
          </pre>
        </DetailBlock>
      )}
      {detail.resultPreview && (
        <DetailBlock label="执行结果">
          <pre className="max-h-[240px] overflow-auto whitespace-pre-wrap break-words font-mono text-[11.5px] leading-relaxed text-ink-muted">
            {detail.resultPreview}
          </pre>
        </DetailBlock>
      )}
      {detail.error && (
        <DetailBlock label="错误详情" danger>
          <pre className="max-h-[200px] overflow-auto whitespace-pre-wrap break-words font-mono text-[11.5px] leading-relaxed text-danger">
            {detail.error}
          </pre>
        </DetailBlock>
      )}
      {detail.recalled && detail.recalled.length > 0 && (
        <DetailBlock label={`相关记忆 ${detail.recalled.length} 条`}>
          <ul className="flex flex-col gap-1">
            {detail.recalled.map((r) => {
              // 可跳转时做成按钮：用户看到「原来想起了这条」之后，下一步几乎
              // 一定想看它到底写了什么。不可跳转（没有回调）时退化为纯文本。
              const body = (
                <>
                  <span className="text-ink">{r.text}</span>
                  {r.via && <span className="ml-1 text-ink-faint">↳ {r.via}</span>}
                </>
              )
              return (
                <li key={r.id} className="text-[11.5px] leading-relaxed text-ink-muted">
                  {onOpenMemoryNode ? (
                    <button
                      type="button"
                      onClick={() => onOpenMemoryNode(r.id)}
                      title="在记忆页中查看这个节点"
                      className="w-full rounded-[4px] px-1 py-0.5 text-left transition-colors hover:bg-surface-raised hover:text-ink"
                    >
                      {body}
                    </button>
                  ) : (
                    body
                  )}
                </li>
              )
            })}
          </ul>
        </DetailBlock>
      )}
      {(detail.verdict || (detail.issues && detail.issues.length > 0)) && (
        <DetailBlock
          label="复核结论"
          danger={detail.verdict !== undefined && detail.verdict !== 'on_track'}
        >
          {detail.verdict && (
            <p className="font-mono text-[11.5px] text-ink-muted">verdict: {detail.verdict}</p>
          )}
          {detail.issues && detail.issues.length > 0 && (
            <ul className="mt-1 list-disc pl-4 text-[11.5px] leading-relaxed text-ink-muted">
              {detail.issues.map((s, i) => (
                <li key={`${i}-${s.slice(0, 16)}`}>{s}</li>
              ))}
            </ul>
          )}
        </DetailBlock>
      )}
    </div>
  )
}

/** DetailBlock 是「标签 + 内容」的小块，统一间距与配色。 */
function DetailBlock({
  label,
  danger,
  children
}: {
  label: string
  danger?: boolean
  children: React.ReactNode
}): React.JSX.Element {
  return (
    <div>
      <div
        className={`mb-0.5 text-[11px] font-semibold uppercase tracking-wide ${
          danger ? 'text-danger' : 'text-ink-faint'
        }`}
      >
        {label}
      </div>
      {children}
    </div>
  )
}
