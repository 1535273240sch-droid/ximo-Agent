import { Zap } from 'lucide-react'

interface GoModeToggleProps {
  /** 是否处于 GO 模式。 */
  value: boolean
  onChange: (next: boolean) => void
}

/**
 * 「GO」开关：打开后，下一次提交会把一段"直接执行、少问确认"的指令
 * 拼进 system_prompt（纯前端字符串拼接，复用现有 SubmitPayload 字段，
 * 后端零改动）。选择只影响之后的提交，可随时开关。
 */
export function GoModeToggle({ value, onChange }: GoModeToggleProps): React.JSX.Element {
  return (
    <button
      type="button"
      onClick={() => onChange(!value)}
      aria-pressed={value}
      title={
        value
          ? 'GO 模式已开启：直接执行，不在低风险步骤上反复确认。点击关闭。'
          : 'GO 模式：直接执行，不在低风险步骤上反复征求确认。点击开启。'
      }
      className={[
        'flex h-8 items-center gap-1 rounded-[6px] border px-2.5 text-[12px] font-semibold tracking-wide transition-colors',
        value
          ? 'border-accent/50 bg-accent/15 text-accent'
          : 'border-border/[0.1] text-ink-muted hover:text-ink'
      ].join(' ')}
    >
      <Zap size={12} className={value ? 'fill-current' : ''} />
      <span>GO</span>
    </button>
  )
}
