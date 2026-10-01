import { AlertTriangle, Check, Loader2, ShieldAlert, X } from 'lucide-react'
import type { PendingApproval } from '../../../store/steps'

/**
 * ApprovalInline 是「工具需要授权」这一步内联的批准 / 拒绝控件（F5）。
 *
 * 它修的是审核报告里最硬的单点问题（C4）：auto_mode=safe 是默认值，只要
 * Agent 想执行任何需确认的工具，旧界面只显示一条没有任何按钮的横幅，run 就
 * 永久停在 waiting_user。这里给出两个按钮，并把「将要执行什么」摆出来 ——
 * 让用户批准一件自己看不到内容的事，等于没有授权。
 */
interface ApprovalInlineProps {
  pending: PendingApproval
  /** 用户做出决定。 */
  onDecide: (approve: boolean) => void
}

export function ApprovalInline({ pending, onDecide }: ApprovalInlineProps): React.JSX.Element {
  const deciding = Boolean(pending.deciding)
  const args = pending.args ?? {}
  const argKeys = Object.keys(args)

  return (
    <div className="mt-1.5 flex flex-col gap-2 rounded-[6px] border border-warning/30 bg-warning/[0.08] p-2.5">
      <div className="flex items-start gap-2">
        <ShieldAlert size={14} className="mt-0.5 shrink-0 text-warning" />
        <div className="min-w-0 flex-1">
          <div className="text-[12.5px] font-semibold text-warning">
            需要你批准：<span className="font-mono">{pending.toolName || '未知工具'}</span>
          </div>
          <p className="mt-0.5 text-[11.5px] leading-relaxed text-ink-muted">
            {pending.message ||
              '该工具属于需要人工确认的风险等级，批准后本次任务才会继续执行。'}
          </p>
          {argKeys.length > 0 && (
            <pre className="mt-1.5 max-h-[140px] overflow-auto whitespace-pre-wrap break-words rounded-[5px] border border-border/[0.08] bg-surface p-2 font-mono text-[11px] leading-relaxed text-ink-muted">
              {JSON.stringify(args, null, 2)}
            </pre>
          )}
        </div>
      </div>

      {pending.decisionError && (
        <div className="flex items-start gap-2 rounded-[5px] border border-danger/30 bg-danger/10 p-2 text-[11.5px] text-danger">
          <AlertTriangle size={12} className="mt-0.5 shrink-0" />
          <span className="break-words">提交决定失败：{pending.decisionError}</span>
        </div>
      )}

      <div className="flex items-center gap-2">
        <button
          type="button"
          disabled={deciding}
          onClick={() => onDecide(true)}
          className={[
            'flex h-7 items-center gap-1.5 rounded-[6px] px-3 text-[12px] font-semibold transition-all',
            deciding ? 'glass-inset cursor-not-allowed text-ink-faint opacity-60' : 'glass-accent-solid'
          ].join(' ')}
        >
          {deciding ? <Loader2 size={12} className="animate-spin" /> : <Check size={12} />}
          <span>批准执行</span>
        </button>
        <button
          type="button"
          disabled={deciding}
          onClick={() => onDecide(false)}
          className={[
            'glass-panel flex h-7 items-center gap-1.5 rounded-[6px] px-3 text-[12px] font-semibold transition-all',
            deciding ? 'cursor-not-allowed text-ink-faint opacity-60' : 'text-ink hover:brightness-110'
          ].join(' ')}
        >
          <X size={12} />
          <span>拒绝</span>
        </button>
        <span className="text-[11px] text-ink-faint">
          拒绝不是失败：模型会收到「用户拒绝执行」并另想办法
        </span>
      </div>
    </div>
  )
}
