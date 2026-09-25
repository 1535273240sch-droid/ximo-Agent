import { useState } from 'react'
import { AlertTriangle, Check, ListChecks, Loader2, RefreshCw } from 'lucide-react'
import type { PlanProposal } from '../../store/app-store'

/**
 * 计划确认卡片（任务4）。
 *
 * 展示后端产出的执行步骤，并给出「确认执行 / 重新规划」两个决定。
 *
 * 为什么把决定交给父组件而不是自己调 store：卡片在 ChatView 里是按 run 渲染
 * 的，由父组件传入 runId 与已绑定的回调，可以让本组件保持无副作用、便于单独
 * 渲染与测试；同时避免在列表渲染中反复读取全局 store。
 */
interface PlanConfirmCardProps {
  plan: PlanProposal
  /** 提交决定：approved 为 true 表示确认执行。 */
  onDecide: (approved: boolean) => void
}

export function PlanConfirmCard({ plan, onDecide }: PlanConfirmCardProps): React.JSX.Element {
  const [expanded, setExpanded] = useState(false)
  const deciding = Boolean(plan.deciding)
  const steps = plan.steps

  return (
    <div className="flex flex-col gap-3 rounded-[10px] border border-accent/30 bg-accent/[0.06] p-4">
      {/* 标题 */}
      <div className="flex items-center gap-2">
        <ListChecks size={16} className="shrink-0 text-accent" />
        <span className="font-display text-[13.5px] font-semibold text-ink">
          执行计划待确认
        </span>
        {plan.revision > 1 && (
          <span className="rounded bg-surface-raised px-1.5 py-0.5 font-mono text-[12px] text-ink-muted border border-border/[0.08]">
            第 {plan.revision} 版
          </span>
        )}
        <div className="flex-1" />
      </div>

      <p className="text-[12.5px] leading-relaxed text-ink-muted">
        确认后才会开始执行。若要调整思路，可要求重新规划。
      </p>

      {/* 步骤列表：解析出步骤时渲染成有序列表，否则回退到渲染全文，
          避免模型输出格式不规整时卡片上什么都不显示。 */}
      {steps.length > 0 ? (
        <ol className="flex flex-col gap-1.5 rounded-[8px] border border-border/[0.08] bg-surface p-3">
          {steps.map((step, i) => (
            <li key={`${i}-${step.slice(0, 24)}`} className="flex items-start gap-2.5">
              <span className="mt-[1px] flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-accent/15 font-mono text-[11.5px] font-semibold text-accent">
                {i + 1}
              </span>
              <span className="whitespace-pre-wrap break-words text-[13px] leading-relaxed text-ink">
                {step}
              </span>
            </li>
          ))}
        </ol>
      ) : (
        <div className="rounded-[8px] border border-border/[0.08] bg-surface p-3">
          <pre className="whitespace-pre-wrap break-words font-ui text-[13px] leading-relaxed text-ink">
            {plan.plan}
          </pre>
        </div>
      )}

      {/* 计划全文：有步骤列表时默认折叠，方便核对原始输出。 */}
      {steps.length > 0 && plan.plan && (
        <div>
          <button
            type="button"
            onClick={() => setExpanded((v) => !v)}
            className="text-[12px] font-medium text-ink-muted transition-colors hover:text-ink"
          >
            {expanded ? '收起计划全文' : '查看计划全文'}
          </button>
          {expanded && (
            <pre className="mt-2 max-h-[260px] overflow-auto whitespace-pre-wrap break-words rounded-[8px] border border-border/[0.08] bg-surface p-3 font-ui text-[12.5px] leading-relaxed text-ink-muted">
              {plan.plan}
            </pre>
          )}
        </div>
      )}

      {/* 提交失败提示 */}
      {plan.decisionError && (
        <div className="flex items-start gap-2 rounded-[8px] border border-danger/30 bg-danger/10 p-2.5 text-[12.5px] text-danger">
          <AlertTriangle size={14} className="mt-0.5 shrink-0" />
          <span className="break-words">提交决定失败：{plan.decisionError}</span>
        </div>
      )}

      {/* 两个决定 */}
      <div className="flex items-center gap-2">
        <button
          type="button"
          disabled={deciding}
          onClick={() => onDecide(true)}
          className={[
            'flex h-8 items-center gap-1.5 rounded-[6px] px-3.5 text-[12.5px] font-semibold transition-all',
            deciding
              ? 'glass-inset cursor-not-allowed text-ink-faint opacity-60'
              : 'glass-accent-solid'
          ].join(' ')}
        >
          {deciding ? (
            <Loader2 size={13} className="animate-spin" />
          ) : (
            <Check size={13} />
          )}
          <span>确认执行</span>
        </button>

        <button
          type="button"
          disabled={deciding}
          onClick={() => onDecide(false)}
          className={[
            'glass-panel flex h-8 items-center gap-1.5 rounded-[6px] px-3.5 text-[12.5px] font-semibold transition-all',
            deciding ? 'cursor-not-allowed text-ink-faint opacity-60' : 'text-ink hover:brightness-110'
          ].join(' ')}
        >
          <RefreshCw size={13} />
          <span>重新规划</span>
        </button>
      </div>
    </div>
  )
}
