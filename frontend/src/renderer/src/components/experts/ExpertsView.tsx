import { useEffect, useMemo, useRef, useState } from 'react'
import { MessageCircle, Search, Wrench, X } from 'lucide-react'
import { useStore } from '../../store/app-store'
import { ExpertCard, ExpertEmoji } from './ExpertCard'
import { ALL_DIVISIONS, DivisionFilter } from './DivisionFilter'
import {
  DIVISION_LABELS,
  EXPERTS,
  SAMPLE_EXPERT_COUNT,
  TOTAL_EXPERT_COUNT,
  type Expert
} from './experts-data'

export function ExpertsView(): React.JSX.Element {
  const [selectedDivision, setSelectedDivision] = useState(ALL_DIVISIONS)
  const [searchQuery, setSearchQuery] = useState('')
  const [activeExpert, setActiveExpert] = useState<Expert | null>(null)

  const activeSessionId = useStore((s) => s.activeSessionId)
  const createSession = useStore((s) => s.createSession)
  const submit = useStore((s) => s.submit)
  const setView = useStore((s) => s.setView)

  const filteredExperts = useMemo(() => {
    const q = searchQuery.trim().toLowerCase()
    return EXPERTS.filter((e) => {
      if (selectedDivision !== ALL_DIVISIONS && e.division !== selectedDivision) {
        return false
      }
      if (!q) return true
      const divisionLabel = (DIVISION_LABELS[e.division] ?? '').toLowerCase()
      return (
        e.name.toLowerCase().includes(q) ||
        e.description.toLowerCase().includes(q) ||
        e.division.toLowerCase().includes(q) ||
        divisionLabel.includes(q)
      )
    })
  }, [selectedDivision, searchQuery])

  // 这里必须带 expert_id，否则后端走的是通用主循环，「以该专家身份开启会话」只
  // 是把一段专家口吻的 system_prompt 喂给主模型——专家库里的编排（规划阶段、
  // 子 Agent、工具链、模型池）一个都不会启动，用户看到的答案是主模型在扮演专家。
  //
  // system_prompt 也不再发：后端专家路径用的是 internal/expert.BuildSystemPrompt
  // 依据同一份专家定义生成的人设提示词（前端这份是它的手抄副本，且后端根本不读
  // SubmitRequest.SystemPrompt），留着只会让两边措辞漂移。
  const handleStartChat = (e: Expert): void => {
    if (!activeSessionId) {
      createSession()
    }
    const prompt = `以${e.name}的专业身份介入，为我分析并规划接下来的工作。`

    void submit(prompt, { expert_id: e.id })
    setView('chat')
  }

  return (
    <div className="flex h-full flex-col overflow-hidden bg-canvas">
      <div className="shrink-0 border-b border-border/[0.08] px-6 py-5 bg-surface">
        <div className="mx-auto flex max-w-[1200px] flex-col gap-4">
          <div className="flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h1 className="font-display text-[22px] font-semibold tracking-tight text-ink">
                专家角色库
              </h1>
              <p className="mt-1 text-[13px] text-ink-muted">
                本页为跨部门精选的 {SAMPLE_EXPERT_COUNT} 位，点击可直接以该专家身份开启会话；专家库共{' '}
                {TOTAL_EXPERT_COUNT} 位，其余专家由「Agent 集群」模式按任务自动选用
              </p>
            </div>

            <div className="glass-inset flex w-full items-center gap-2 px-3 py-1.5 sm:w-[280px]">
              <Search size={14} className="shrink-0 text-ink-muted" />
              <input
                type="text"
                value={searchQuery}
                onChange={(e) => setSearchQuery(e.target.value)}
                placeholder="搜索专家名称、能力或领域…"
                className="w-full bg-transparent text-[13px] text-ink outline-none placeholder:text-ink-faint"
              />
              {searchQuery && (
                <button
                  type="button"
                  onClick={() => setSearchQuery('')}
                  className="text-ink-muted hover:text-ink"
                >
                  <X size={14} />
                </button>
              )}
            </div>
          </div>

          <DivisionFilter selected={selectedDivision} onSelect={setSelectedDivision} />
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-6 py-6">
        <div className="mx-auto max-w-[1200px]">
          {filteredExperts.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-20 text-center select-none">
              <p className="text-[14px] font-medium text-ink">未找到匹配的专家</p>
              <p className="mt-1 text-[12.5px] text-ink-muted">
                请尝试更换关键词，或切换至「全部部门」查看完整名单。
              </p>
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3">
              {filteredExperts.map((expert) => (
                <ExpertCard
                  key={expert.id}
                  expert={expert}
                  onClick={() => setActiveExpert(expert)}
                />
              ))}
            </div>
          )}
        </div>
      </div>

      {activeExpert && (
        <ExpertDetailDrawer
          expert={activeExpert}
          onClose={() => setActiveExpert(null)}
          onStartChat={handleStartChat}
        />
      )}
    </div>
  )
}

function ExpertDetailDrawer({
  expert,
  onClose,
  onStartChat
}: {
  expert: Expert
  onClose: () => void
  onStartChat: (e: Expert) => void
}): React.JSX.Element {
  const drawerRef = useRef<HTMLDivElement>(null)
  const divisionLabel = DIVISION_LABELS[expert.division] ?? expert.division

  useEffect(() => {
    const handleKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handleKey)
    return () => window.removeEventListener('keydown', handleKey)
  }, [onClose])

  return (
    <div
      role="dialog"
      aria-modal="true"
      onClick={onClose}
      className="fixed inset-0 z-50 flex justify-end bg-canvas/60 backdrop-blur-[2px]"
    >
      <div
        ref={drawerRef}
        onClick={(e) => e.stopPropagation()}
        className="glass-panel animate-glass-in flex h-full w-full max-w-[460px] flex-col overflow-hidden border-l border-border/[0.12] bg-surface shadow-card"
      >
        <div className="flex items-center justify-between border-b border-border/[0.08] px-5 py-4">
          <span className="text-[12px] font-semibold uppercase tracking-wider text-ink-muted">
            专家档案
          </span>
          <button
            type="button"
            onClick={onClose}
            className="flex h-7 w-7 items-center justify-center rounded-[6px] text-ink-muted hover:bg-surface-raised hover:text-ink transition-colors"
          >
            <X size={15} />
          </button>
        </div>

        <div className="min-h-0 flex-1 overflow-y-auto p-6 flex flex-col gap-5">
          <div className="flex items-start gap-4">
            <ExpertEmoji emoji={expert.emoji} color={expert.color} size="lg" />
            <div className="min-w-0 flex-1">
              <h2 className="font-display text-[18px] font-semibold text-ink">
                {expert.name}
              </h2>
              <p className="mt-1 font-mono text-[12px] text-accent font-medium uppercase tracking-wider">
                {divisionLabel} · {expert.division}
              </p>
            </div>
          </div>

          <div className="flex flex-col gap-2">
            <h3 className="text-[12px] font-semibold uppercase tracking-wider text-ink-muted">
              核心专业职责
            </h3>
            <p className="text-[13.5px] leading-relaxed text-ink font-normal">
              {expert.description}
            </p>
          </div>

          {expert.vibe && (
            <div className="rounded-[8px] border border-border/[0.08] bg-canvas p-3.5">
              <h3 className="text-[12px] font-semibold uppercase tracking-wider text-ink-muted mb-1">
                思考与执业特质
              </h3>
              <p className="text-[13px] italic leading-relaxed text-ink-muted">
                "{expert.vibe}"
              </p>
            </div>
          )}

          {expert.tools && expert.tools.length > 0 && (
            <div className="flex flex-col gap-2">
              <h3 className="text-[12px] font-semibold uppercase tracking-wider text-ink-muted flex items-center gap-1.5">
                <Wrench size={12} className="text-accent" />
                <span>关联工具能力</span>
              </h3>
              <div className="flex flex-wrap gap-1.5">
                {expert.tools.map((tool) => (
                  <span
                    key={tool}
                    className="rounded bg-canvas px-2.5 py-1 font-mono text-[12px] text-ink border border-border/[0.1]"
                  >
                    {tool}
                  </span>
                ))}
              </div>
            </div>
          )}
        </div>

        <div className="border-t border-border/[0.08] p-4 bg-surface-raised">
          <button
            type="button"
            onClick={() => onStartChat(expert)}
            className="glass-accent-solid flex w-full items-center justify-center gap-2 py-2.5 text-[13.5px] font-medium"
          >
            <MessageCircle size={15} />
            <span>以该专家身份开启会话</span>
          </button>
        </div>
      </div>
    </div>
  )
}
