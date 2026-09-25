import { ListChecks } from 'lucide-react'

interface PlanModeToggleProps {
  /** 是否开启计划模式。 */
  value: boolean
  onChange: (next: boolean) => void
  /** 置灰禁用（例如已选择专家时，计划确认门不适用）。 */
  disabled?: boolean
  /** 禁用时的 title 提示文案；不传则用通用提示。 */
  disabledTitle?: string
}

/**
 * 「计划」开关（任务4）：打开后，下一次提交会带上 plan_mode=true，
 * 后端先产出一份执行计划并停在计划卡片上，用户确认后才开始执行。
 *
 * 与 GO 模式的区别：GO 是"少问确认、直接执行"，计划模式是"先看清楚打算怎么做
 * 再执行"，两者用途相反，因此各自独立成开关而不是合并。选择只影响之后的提交。
 */
export function PlanModeToggle({
  value,
  onChange,
  disabled = false,
  disabledTitle
}: PlanModeToggleProps): React.JSX.Element {
  const title = disabled
    ? (disabledTitle ?? '计划模式当前不可用')
    : value
      ? '计划模式已开启：提交后先给出执行计划，确认后才开始执行。点击关闭。'
      : '计划模式：先给出执行计划，等你确认后再开始执行。点击开启。'
  return (
    <button
      type="button"
      onClick={() => onChange(!value)}
      aria-pressed={value}
      disabled={disabled}
      title={title}
      className={[
        'flex h-8 items-center gap-1 rounded-[6px] border px-2.5 text-[12px] font-semibold tracking-wide transition-colors',
        value
          ? 'border-accent/50 bg-accent/15 text-accent'
          : 'border-border/[0.1] text-ink-muted hover:text-ink',
        disabled ? 'cursor-not-allowed opacity-40' : ''
      ].join(' ')}
    >
      <ListChecks size={12} />
      <span>计划</span>
    </button>
  )
}
