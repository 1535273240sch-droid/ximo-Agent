import { AlertTriangle, Check, CircleHelp, Loader2, X } from 'lucide-react'
import type { ClosureCheck, ClosureReport } from '@shared/types'

/**
 * ClosureBadge 是「这次任务到底做完了没有」的确定性结论。
 *
 * 它存在的理由就是审核报告开头那句话：状态机仍然只说 completed / failed，
 * 而「跑满轮数被强制收尾」以前和「正常完成」在界面上完全一样。闭环结论由后端
 * 的确定性检查算出（不额外花模型调用），前端只负责如实显示，并提供「继续」按钮
 * 把未通过检查项写回给用户。
 */
interface ClosureBadgeProps {
  closure: ClosureReport
  /** 点击「继续」时把未通过检查项的说明传给上层。 */
  onContinue?: () => void
  /** 正在提交"继续"。 */
  continuing?: boolean
}

/** VERDICT_META 把 verdict 映射成文案、配色与图标。 */
const VERDICT_META: Record<
  ClosureReport['verdict'],
  { label: string; tone: string; Icon: typeof Check; hint: string }
> = {
  closed: {
    label: '已闭环',
    tone: 'text-success border-success/30 bg-success/10',
    Icon: Check,
    hint: '所有适用检查均通过'
  },
  partial: {
    label: '部分完成',
    tone: 'text-warning border-warning/30 bg-warning/10',
    Icon: AlertTriangle,
    hint: '任务结束了，但有检查未通过'
  },
  needs_user: {
    label: '需要你决定',
    tone: 'text-warning border-warning/30 bg-warning/10',
    Icon: CircleHelp,
    hint: '任务停在一个需要你确认的地方'
  },
  failed: {
    label: '失败',
    tone: 'text-danger border-danger/30 bg-danger/10',
    Icon: X,
    hint: '本次没有可用的最终答案'
  }
}

/** VERDICT_META 的兜底：后端新增 verdict 取值时旧前端不应白屏。 */
function metaFor(verdict: ClosureReport['verdict']): (typeof VERDICT_META)[ClosureReport['verdict']] {
  return VERDICT_META[verdict] ?? VERDICT_META.partial
}

export function ClosureBadge({ closure, onContinue, continuing }: ClosureBadgeProps): React.JSX.Element {
  const meta = metaFor(closure.verdict)
  const { Icon } = meta
  const failed = closure.checks.filter((c) => !c.pass)
  const canContinue = closure.verdict === 'partial' && Boolean(onContinue)

  return (
    <div className="flex flex-col gap-2 border-t border-border/[0.08] px-3 py-2.5">
      <div className="flex items-center gap-2">
        <span className="text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
          闭环校验
        </span>
        <div className="flex-1" />
        <span
          className={`flex items-center gap-1.5 rounded-[5px] border px-2 py-0.5 text-[11.5px] font-semibold ${meta.tone}`}
          title={meta.hint}
        >
          <Icon size={11} />
          {meta.label}
        </span>
      </div>

      {closure.checks.length > 0 && (
        <ul className="flex flex-wrap gap-x-3.5 gap-y-1" role="list">
          {closure.checks.map((c) => (
            <CheckLine key={c.id} check={c} />
          ))}
        </ul>
      )}

      {failed.length > 0 && (
        <p className="text-[11.5px] leading-relaxed text-ink-muted">
          未通过：
          {failed.map((c) => (c.note ? `${c.label}（${c.note}）` : c.label)).join('；')}
        </p>
      )}

      {canContinue && (
        <div className="flex items-center gap-2">
          <button
            type="button"
            disabled={continuing}
            onClick={onContinue}
            className={[
              'flex h-7 items-center gap-1.5 rounded-[6px] px-3 text-[12px] font-semibold transition-all',
              continuing ? 'glass-inset cursor-not-allowed text-ink-faint opacity-60' : 'glass-panel text-ink hover:brightness-110'
            ].join(' ')}
          >
            {continuing && <Loader2 size={12} className="animate-spin" />}
            <span>继续完成</span>
          </button>
          <span className="text-[11.5px] text-ink-faint">
            会以上一次未完成的部分作为新任务继续执行
          </span>
        </div>
      )}
    </div>
  )
}

/** CheckLine 渲染一条检查项。 */
function CheckLine({ check }: { check: ClosureCheck }): React.JSX.Element {
  return (
    <li
      className={`flex items-center gap-1 text-[11.5px] ${check.pass ? 'text-ink-muted' : 'text-danger'}`}
      title={check.note}
    >
      {check.pass ? (
        <Check size={11} className="shrink-0 text-success" />
      ) : (
        <X size={11} className="shrink-0" />
      )}
      <span>{check.label}</span>
    </li>
  )
}
