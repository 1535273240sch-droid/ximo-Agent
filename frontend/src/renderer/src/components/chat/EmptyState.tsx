import { BookOpen, FileText, Sparkles, Terminal } from 'lucide-react'

const EXAMPLES = [
  {
    icon: FileText,
    label: '分析项目架构与交付物',
    prompt: '分析当前项目代码库中已完成与未完成的模块，并列出关键交付差距。'
  },
  {
    icon: Terminal,
    label: '代码巡检与质量重构',
    prompt: '检查仓库中的数据竞争与并发问题，提出符合工业级标准的重构方案。'
  },
  {
    icon: BookOpen,
    label: '检索领域知识库与规范',
    prompt: '检索系统内部知识库中关于多进程崩溃恢复与事务幂等性的设计规范。'
  },
  {
    icon: Sparkles,
    label: '多智能体任务拆解',
    prompt: '将以下业务目标拆解为可被专业子代理独立并发执行的工单任务链。'
  }
]

interface EmptyStateProps {
  onSelectPrompt: (prompt: string) => void
}

export function EmptyState({ onSelectPrompt }: EmptyStateProps): React.JSX.Element {
  return (
    <div className="flex flex-col items-center justify-center py-16 text-center select-none">
      <div className="flex h-12 w-12 items-center justify-center rounded-[10px] border border-border/[0.12] bg-surface mb-4">
        <Sparkles size={22} className="text-accent" />
      </div>

      <h2 className="font-display text-[22px] font-semibold tracking-tight text-ink">
        XimoAgent
      </h2>
      <p className="mt-1.5 max-w-[460px] text-[13.5px] leading-relaxed text-ink-muted">
        高并发自愈架构驱动的全能智能体工作台。可自主调用本地工具链、调度进程池并维护持久化执行状态。
      </p>

      <div className="mt-10 w-full max-w-[620px]">
        <div className="mb-3 text-[12px] font-semibold tracking-wider text-ink-faint uppercase text-left pl-1">
          建议探索场景
        </div>
        <div className="grid grid-cols-1 gap-2.5 sm:grid-cols-2">
          {EXAMPLES.map(({ icon: Icon, label, prompt }) => (
            <button
              key={label}
              type="button"
              onClick={() => onSelectPrompt(prompt)}
              className="glass-panel glass-panel-hover flex items-start gap-3 p-3.5 text-left transition-colors"
            >
              <div className="mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-[6px] border border-border/[0.1] bg-surface-raised text-accent">
                <Icon size={13} />
              </div>
              <div className="flex flex-col gap-0.5">
                <span className="text-[13px] font-medium text-ink">{label}</span>
                <span className="line-clamp-2 text-[12px] leading-relaxed text-ink-muted">
                  {prompt}
                </span>
              </div>
            </button>
          ))}
        </div>
      </div>
    </div>
  )
}
