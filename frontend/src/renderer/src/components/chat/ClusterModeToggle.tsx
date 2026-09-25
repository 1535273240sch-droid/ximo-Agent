import { Users } from 'lucide-react'
import { CLUSTER_MODE_SIZE } from '@shared/types'

interface ClusterModeToggleProps {
  /** 是否处于集群模式。 */
  value: boolean
  onChange: (next: boolean) => void
  /** 已手选专家时禁用：专家是单点执行，与集群的"自动组队"语义冲突。 */
  disabled?: boolean
  disabledTitle?: string
}

/**
 * 「集群」开关：打开后，下一次提交会以 Agent 集群模式执行 —— 引擎按任务
 * 内容自动挑选 CLUSTER_MODE_SIZE 位专家子代理并行处理，最后汇总成一份
 * 多视角报告。
 *
 * 数量来自后端 expert_agent 资源闸门的容量，与设置页「子代理模型分配」
 * 里对用户的说明「同时最多 8 个」一致，因此这里不提供可调数字：调高没有
 * 意义，超出的调用只在闸门里排队。
 */
export function ClusterModeToggle({
  value,
  onChange,
  disabled,
  disabledTitle
}: ClusterModeToggleProps): React.JSX.Element {
  const title = disabled
    ? disabledTitle
    : value
      ? `Agent 集群已开启：本次任务将由 ${CLUSTER_MODE_SIZE} 位专家并行处理。点击关闭。`
      : `Agent 集群：让引擎自动挑选 ${CLUSTER_MODE_SIZE} 位专家并行处理本次任务，再汇总产出。点击开启。`

  const tone = disabled
    ? 'cursor-not-allowed border-border/[0.1] text-ink-faint'
    : value
      ? 'border-accent/50 bg-accent/15 text-accent'
      : 'border-border/[0.1] text-ink-muted hover:text-ink'

  return (
    <button
      type="button"
      onClick={() => onChange(!value)}
      disabled={disabled}
      aria-pressed={value}
      title={title}
      className={`flex h-8 items-center gap-1 rounded-[6px] border px-2.5 text-[12px] font-semibold tracking-wide transition-colors ${tone}`}
    >
      <Users size={12} className={value && !disabled ? 'fill-current' : ''} />
      <span>集群</span>
      {value && !disabled && (
        <span className="text-[10px] opacity-70">{CLUSTER_MODE_SIZE}</span>
      )}
    </button>
  )
}
