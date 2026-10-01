import { useEffect, useMemo, useState } from 'react'
import {
  AlertCircle,
  Loader2,
  MessageCircle,
  Pencil,
  Plus,
  Search,
  Trash2,
  Wrench,
  X
} from 'lucide-react'
import type { ExpertCardPayload } from '@shared/types'
import { useStore } from '../../store/app-store'
import {
  bridgeFromWindow,
  divisionCounts,
  filterExperts,
  setExpertsBridge,
  toolCatalogue,
  useExpertsStore
} from '../../store/experts-store'
import { ExpertCard, ExpertEmoji } from './ExpertCard'
import { ALL_DIVISIONS, DivisionFilter } from './DivisionFilter'
import { divisionLabel, type Expert } from './experts-data'

/** 表单控件的统一外观（与知识页/记忆页的输入框同一套玻璃语言）。 */
const FIELD_CLASS =
  'glass-inset w-full px-3 py-2 text-[13px] text-ink outline-none placeholder:text-ink-faint'
const FIELD_LABEL_CLASS =
  'text-[12px] font-semibold uppercase tracking-wider text-ink-muted'

/**
 * 专家角色库。
 *
 * 数据只有一个来源：store/experts-store（背后是 window.ximo.expertList）。v2.5
 * 的版本读渲染层内联的 60 位样本，于是「共 254 位」的文案与列表实际条数对不上，
 * 自定义专家也没有任何入口 —— 两件事的根因都是前端自持一份目录。
 */
export function ExpertsView(): React.JSX.Element {
  const experts = useExpertsStore((s) => s.experts)
  const divisions = useExpertsStore((s) => s.divisions)
  const loading = useExpertsStore((s) => s.loading)
  const error = useExpertsStore((s) => s.error)
  const loaded = useExpertsStore((s) => s.loaded)
  const load = useExpertsStore((s) => s.load)
  const saveExpert = useExpertsStore((s) => s.save)
  const removeExpert = useExpertsStore((s) => s.remove)

  const [selectedDivision, setSelectedDivision] = useState(ALL_DIVISIONS)
  const [searchQuery, setSearchQuery] = useState('')
  const [customOnly, setCustomOnly] = useState(false)
  const [activeExpertId, setActiveExpertId] = useState<string | null>(null)
  // undefined = 表单关闭；null = 新建；Expert = 编辑该自定义专家。
  const [formTarget, setFormTarget] = useState<Expert | null | undefined>(undefined)

  const activeSessionId = useStore((s) => s.activeSessionId)
  const createSession = useStore((s) => s.createSession)
  const submit = useStore((s) => s.submit)
  const setView = useStore((s) => s.setView)

  // 桥注入：挂载时把 window.ximo 交给 store；卸载时清空，避免测试之间互相污染。
  useEffect(() => {
    setExpertsBridge(bridgeFromWindow())
    return () => setExpertsBridge(undefined)
  }, [])

  // 懒加载：只在从未装载成功时拉一次目录；切走再切回来不重复打 IPC。
  // 失败时不置 loaded，所以用户重新进入这一页会自然重试。
  useEffect(() => {
    if (!loaded) void load()
  }, [loaded, load])

  const counts = useMemo(() => divisionCounts(experts), [experts])
  const customCount = useMemo(() => experts.filter((e) => e.custom === true).length, [experts])
  const toolOptions = useMemo(() => toolCatalogue(experts), [experts])

  const filteredExperts = useMemo(
    () => filterExperts(experts, { division: selectedDivision, query: searchQuery, customOnly }),
    [experts, selectedDivision, searchQuery, customOnly]
  )

  // 抽屉按 id 定位而不是持有对象：保存后 store 会重新装载目录，持有的那份立刻过期
  // （编辑完抽屉还显示旧文案）；删除后还会留下一个点不动的幽灵。
  const activeExpert = useMemo(
    () => experts.find((e) => e.id === activeExpertId) ?? null,
    [experts, activeExpertId]
  )

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

  /** 保存失败时把异常继续抛给表单：后端的中文原文要就地显示在表单里。 */
  const handleSaveExpert = async (card: ExpertCardPayload): Promise<void> => {
    await saveExpert(card)
    setFormTarget(undefined)
  }

  const summary = loaded
    ? `共 ${experts.length} 位专家，其中 ${customCount} 位为你自定义`
    : loading
      ? '正在加载专家目录…'
      : '专家目录尚未加载'

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
                {summary}，点击卡片可查看档案或以该专家身份开启会话
              </p>
            </div>

            <div className="flex items-center gap-2">
              <button
                type="button"
                onClick={() => setFormTarget(null)}
                className="glass-accent-solid flex shrink-0 items-center gap-1.5 px-3 py-1.5 text-[12.5px] font-medium"
              >
                <Plus size={14} />
                <span>新建专家</span>
              </button>

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
          </div>

          <div className="flex items-center gap-2">
            <div className="min-w-0 flex-1">
              <DivisionFilter
                divisions={divisions}
                counts={counts}
                total={experts.length}
                selected={selectedDivision}
                onSelect={setSelectedDivision}
              />
            </div>

            <button
              type="button"
              onClick={() => setCustomOnly((v) => !v)}
              title="只显示你自己创建的专家（可编辑 / 可删除）"
              className={`flex shrink-0 items-center gap-1.5 rounded-subtle border px-3 py-1 text-[12.5px] font-medium transition-colors ${
                customOnly
                  ? 'border-accent bg-accent/15 text-accent'
                  : 'border-border/[0.08] bg-canvas text-ink-muted hover:text-ink'
              }`}
            >
              <span>仅看自定义</span>
              <span className="font-mono text-[12px] opacity-80">({customCount})</span>
            </button>
          </div>
        </div>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-6 py-6">
        <div className="mx-auto flex max-w-[1200px] flex-col gap-4">
          {/* 失败原因必须显眼：静默的空列表会让用户以为目录里真的什么都没有。 */}
          {error && (
            <div className="flex items-start gap-2 rounded-panel border border-danger/30 bg-danger/[0.06] p-3 text-[12.5px] text-ink">
              <AlertCircle size={15} className="mt-0.5 shrink-0 text-danger" />
              <span className="flex-1">{error}</span>
              {/* 应用启动时后端可能还没就绪，首次装载会失败。没有这个按钮，用户只能
                  靠切走再切回来触发重试 —— 那不是一个能发现的操作。 */}
              <button
                type="button"
                onClick={() => void load()}
                disabled={loading}
                className="shrink-0 rounded-subtle border border-border/[0.1] px-2.5 py-1 text-[12px] font-medium text-ink-muted transition-colors hover:bg-surface hover:text-ink disabled:opacity-50"
              >
                重试
              </button>
            </div>
          )}

          {loading && experts.length === 0 ? (
            <div className="flex flex-col items-center justify-center gap-2 py-20 text-center select-none">
              <Loader2 size={20} className="animate-spin text-accent" />
              <p className="text-[13px] text-ink-muted">正在加载专家目录…</p>
            </div>
          ) : filteredExperts.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-20 text-center select-none">
              <p className="text-[14px] font-medium text-ink">
                {experts.length > 0
                  ? '未找到匹配的专家'
                  : error
                    ? '专家目录加载失败'
                    : '专家目录里还没有内容'}
              </p>
              <p className="mt-1 text-[12.5px] text-ink-muted">
                {experts.length > 0
                  ? '请尝试更换关键词，或切换至「全部部门」查看完整名单。'
                  : error
                    ? '请查看上方的错误信息，或点击「重试」重新加载。'
                    : '点击右上角「新建专家」创建第一位自定义专家。'}
              </p>
            </div>
          ) : (
            <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 lg:grid-cols-3">
              {filteredExperts.map((expert) => (
                <ExpertCard
                  key={expert.id}
                  expert={expert}
                  onClick={() => setActiveExpertId(expert.id)}
                />
              ))}
            </div>
          )}
        </div>
      </div>

      {activeExpert && (
        <ExpertDetailDrawer
          expert={activeExpert}
          onClose={() => setActiveExpertId(null)}
          onStartChat={handleStartChat}
          onEdit={(e) => setFormTarget(e)}
          onDelete={removeExpert}
        />
      )}

      {formTarget !== undefined && (
        <ExpertFormModal
          // 换一位专家（或从编辑切到新建）时必须重建表单：否则 useState 的初值
          // 仍是上一位专家的内容。
          key={formTarget?.id ?? 'new'}
          editing={formTarget}
          divisions={divisions}
          toolOptions={toolOptions}
          onSubmit={handleSaveExpert}
          onClose={() => setFormTarget(undefined)}
        />
      )}
    </div>
  )
}

function ExpertDetailDrawer({
  expert,
  onClose,
  onStartChat,
  onEdit,
  onDelete
}: {
  expert: Expert
  onClose: () => void
  onStartChat: (e: Expert) => void
  onEdit: (e: Expert) => void
  onDelete: (id: string) => Promise<boolean>
}): React.JSX.Element {
  const [confirmDelete, setConfirmDelete] = useState(false)
  const [deleting, setDeleting] = useState(false)
  // 只在用户真的点过删除之后才显示 store 里的 error：否则一个陈旧的加载错误会被
  // 误读成「删除失败」。
  const [attempted, setAttempted] = useState(false)
  const storeError = useExpertsStore((s) => s.error)
  const isCustom = expert.custom === true

  useEffect(() => {
    const handleKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', handleKey)
    return () => window.removeEventListener('keydown', handleKey)
  }, [onClose])

  const handleDelete = async (): Promise<void> => {
    setDeleting(true)
    setAttempted(true)
    try {
      await onDelete(expert.id)
      // 删成功后这位专家会从目录里消失，父组件的 activeExpert 随之变成 null，抽屉
      // 自己关闭——这里不需要额外通知。
    } catch {
      // 后端原文已经由 store 写进 error，下面直接显示它（不吞异常是 store 的职责）。
    } finally {
      setDeleting(false)
      setConfirmDelete(false)
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      onClick={onClose}
      className="fixed inset-0 z-50 flex justify-end bg-canvas/60 backdrop-blur-[2px]"
    >
      <div
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
              <div className="flex items-center gap-2">
                <h2 className="font-display text-[18px] font-semibold text-ink">
                  {expert.name}
                </h2>
                {isCustom && (
                  <span className="shrink-0 rounded border border-accent/30 bg-accent/15 px-1.5 py-0.5 text-[10.5px] font-medium text-accent">
                    自定义
                  </span>
                )}
              </div>
              <p className="mt-1 font-mono text-[12px] text-accent font-medium uppercase tracking-wider">
                {divisionLabel(expert.division)} · {expert.division}
              </p>
            </div>
          </div>

          <div className="flex flex-col gap-2">
            <h3 className={FIELD_LABEL_CLASS}>核心专业职责</h3>
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

          {expert.personality && (
            <div className="flex flex-col gap-2">
              <h3 className={FIELD_LABEL_CLASS}>人格提示词</h3>
              <p className="text-[13px] leading-relaxed text-ink-muted whitespace-pre-wrap">
                {expert.personality}
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

        <div className="border-t border-border/[0.08] p-4 bg-surface-raised flex flex-col gap-2">
          {attempted && storeError && (
            <div className="flex items-start gap-2 rounded-subtle border border-danger/30 bg-danger/10 p-2.5 text-[12.5px] text-ink">
              <AlertCircle size={14} className="mt-0.5 shrink-0 text-danger" />
              <span>{storeError}</span>
            </div>
          )}

          {isCustom &&
            (confirmDelete ? (
              <div className="flex items-center gap-2 rounded-subtle border border-danger/30 bg-danger/10 px-3 py-2">
                <span className="flex-1 text-[12.5px] font-medium text-danger">
                  确认删除「{expert.name}」？此操作不可撤销。
                </span>
                <button
                  type="button"
                  onClick={() => void handleDelete()}
                  disabled={deleting}
                  className="flex items-center gap-1 rounded bg-danger px-2.5 py-1 text-[12px] font-medium text-white disabled:opacity-50"
                >
                  {deleting && <Loader2 size={12} className="animate-spin" />}
                  <span>确认删除</span>
                </button>
                <button
                  type="button"
                  onClick={() => setConfirmDelete(false)}
                  disabled={deleting}
                  className="px-1 text-[12px] text-ink-muted hover:text-ink disabled:opacity-50"
                >
                  取消
                </button>
              </div>
            ) : (
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => onEdit(expert)}
                  className="flex flex-1 items-center justify-center gap-1.5 rounded-subtle border border-border/[0.1] px-3 py-2 text-[12.5px] font-medium text-ink-muted transition-colors hover:bg-surface hover:text-ink"
                >
                  <Pencil size={13} />
                  <span>编辑</span>
                </button>
                <button
                  type="button"
                  onClick={() => setConfirmDelete(true)}
                  className="flex flex-1 items-center justify-center gap-1.5 rounded-subtle border border-border/[0.1] px-3 py-2 text-[12.5px] font-medium text-danger transition-colors hover:bg-danger/10"
                >
                  <Trash2 size={13} />
                  <span>删除</span>
                </button>
              </div>
            ))}

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

/**
 * 新建 / 编辑自定义专家的表单。
 *
 * 保存后不做任何本地拼接：store 会重新向服务端要一份目录（ID 由后端生成、Custom
 * 由后端强制），界面因此永远显示后端真实存下来的那条记录。
 */
function ExpertFormModal({
  editing,
  divisions,
  toolOptions,
  onSubmit,
  onClose
}: {
  /** null 表示新建。 */
  editing: Expert | null
  divisions: string[]
  toolOptions: string[]
  onSubmit: (card: ExpertCardPayload) => Promise<void>
  onClose: () => void
}): React.JSX.Element {
  const [name, setName] = useState(editing?.name ?? '')
  const [division, setDivision] = useState(editing?.division ?? '')
  const [description, setDescription] = useState(editing?.description ?? '')
  const [emoji, setEmoji] = useState(editing?.emoji ?? '')
  const [vibe, setVibe] = useState(editing?.vibe ?? '')
  const [personality, setPersonality] = useState(editing?.personality ?? '')
  const [color, setColor] = useState(editing?.color ?? '')
  const [tools, setTools] = useState<string[]>(() => [...(editing?.tools ?? [])])
  const [newTool, setNewTool] = useState('')
  const [busy, setBusy] = useState(false)
  const [formError, setFormError] = useState<string | undefined>(undefined)

  // 勾选清单 = 目录里出现过的工具 ∪ 本人已选的（可能不在目录里）∪ 刚手输的。
  // 并集必须排序：目录一刷新（保存后重新装载）顺序变了，勾选框会整体跳动。
  const toolChoices = useMemo(() => {
    const extra = tools.filter((t) => !toolOptions.includes(t))
    return [...new Set([...toolOptions, ...extra])].sort()
  }, [toolOptions, tools])

  const toggleTool = (tool: string): void => {
    setTools((prev) =>
      prev.includes(tool) ? prev.filter((t) => t !== tool) : [...prev, tool]
    )
  }

  const addTool = (): void => {
    const trimmed = newTool.trim()
    if (trimmed === '') return
    setTools((prev) => (prev.includes(trimmed) ? prev : [...prev, trimmed]))
    setNewTool('')
  }

  const handleSubmit = async (): Promise<void> => {
    const trimmedName = name.trim()
    const trimmedDivision = division.trim()
    // 前端先做一次同样的校验只是为了少一次往返；后端才是权威（错误原文照样透出）。
    if (trimmedName === '') {
      setFormError('专家名称不能为空')
      return
    }
    if (trimmedDivision === '') {
      setFormError('专家部门不能为空（用于工具推荐与界面分组）')
      return
    }

    setBusy(true)
    setFormError(undefined)
    try {
      await onSubmit({
        // 新建时留空：后端会按名称生成稳定的 custom-<slug> ID（随机 ID 会让
        // 「再次编辑同一位专家」变成两件不同的记录）。
        id: editing?.id ?? '',
        division: trimmedDivision,
        name: trimmedName,
        description: description.trim(),
        emoji: emoji.trim() || undefined,
        vibe: vibe.trim() || undefined,
        personality: personality.trim() || undefined,
        color: color.trim() || undefined,
        tools,
        custom: true
      })
    } catch (err) {
      // 后端的中文错误原文（覆盖内置专家 / 名称超长 / 工具不可用…）就地显示。
      setFormError(err instanceof Error ? err.message : String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div
      role="dialog"
      aria-modal="true"
      onClick={onClose}
      className="fixed inset-0 z-50 flex items-center justify-center bg-canvas/60 p-6 backdrop-blur-[2px]"
    >
      <div
        onClick={(e) => e.stopPropagation()}
        className="glass-panel animate-glass-in flex max-h-full w-full max-w-[620px] flex-col overflow-hidden rounded-panel border border-border/[0.12] bg-surface shadow-card"
      >
        <div className="flex items-center justify-between border-b border-border/[0.08] px-5 py-4">
          <div className="min-w-0">
            <span className="font-display text-[15px] font-semibold text-ink">
              {editing ? '编辑自定义专家' : '新建自定义专家'}
            </span>
            <p className="mt-0.5 truncate font-mono text-[11.5px] text-ink-faint">
              {editing ? editing.id : 'ID 由后端按名称自动生成（custom-<名称>）'}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="flex h-7 w-7 shrink-0 items-center justify-center rounded-[6px] text-ink-muted hover:bg-surface-raised hover:text-ink transition-colors"
          >
            <X size={15} />
          </button>
        </div>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-5">
          <div className="flex flex-col gap-1.5">
            <label className={FIELD_LABEL_CLASS}>名称（必填）</label>
            <input
              type="text"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="例如：我的代码审查搭档"
              className={FIELD_CLASS}
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <label className={FIELD_LABEL_CLASS}>所属部门（必填）</label>
            <div className="flex flex-col gap-2 sm:flex-row">
              {/* 下拉列出目录里已有的部门，右侧输入框允许写一个全新的 slug
                  —— 后端不限制部门取值，界面就不该把它锁死成枚举。 */}
              <select
                value={divisions.includes(division) ? division : ''}
                onChange={(e) => setDivision(e.target.value)}
                className={`${FIELD_CLASS} sm:w-[240px]`}
              >
                <option value="">选择已有部门…</option>
                {divisions.map((d) => (
                  <option key={d} value={d}>
                    {divisionLabel(d)}（{d}）
                  </option>
                ))}
              </select>
              <input
                type="text"
                value={division}
                onChange={(e) => setDivision(e.target.value)}
                placeholder="或直接输入新的部门 slug"
                className={FIELD_CLASS}
              />
            </div>
          </div>

          <div className="flex flex-col gap-1.5">
            <label className={FIELD_LABEL_CLASS}>专业职责描述</label>
            <textarea
              value={description}
              onChange={(e) => setDescription(e.target.value)}
              rows={3}
              placeholder="这位专家负责什么、擅长什么"
              className={`${FIELD_CLASS} resize-y leading-relaxed`}
            />
          </div>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <label className={FIELD_LABEL_CLASS}>头像 emoji</label>
              <input
                type="text"
                value={emoji}
                onChange={(e) => setEmoji(e.target.value)}
                maxLength={4}
                placeholder="🛠️"
                className={FIELD_CLASS}
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <label className={FIELD_LABEL_CLASS}>卡片配色</label>
              <input
                type="text"
                value={color}
                onChange={(e) => setColor(e.target.value)}
                placeholder="#3B82F6 或 blue"
                className={FIELD_CLASS}
              />
            </div>
          </div>

          <div className="flex flex-col gap-1.5">
            <label className={FIELD_LABEL_CLASS}>执业风格（vibe）</label>
            <input
              type="text"
              value={vibe}
              onChange={(e) => setVibe(e.target.value)}
              placeholder="一句话概括这位专家的做事方式"
              className={FIELD_CLASS}
            />
          </div>

          <div className="flex flex-col gap-1.5">
            <label className={FIELD_LABEL_CLASS}>人格提示词</label>
            <textarea
              value={personality}
              onChange={(e) => setPersonality(e.target.value)}
              rows={3}
              placeholder="下发给子 Agent 的人设描述（可留空）"
              className={`${FIELD_CLASS} resize-y leading-relaxed`}
            />
          </div>

          <div className="flex flex-col gap-2">
            <div className="flex items-center justify-between">
              <label className={FIELD_LABEL_CLASS}>
                <span className="inline-flex items-center gap-1.5">
                  <Wrench size={12} className="text-accent" />
                  <span>推荐工具</span>
                </span>
              </label>
              <span className="font-mono text-[11.5px] text-ink-faint">
                已选 {tools.length} 个
              </span>
            </div>

            {toolChoices.length === 0 ? (
              <p className="text-[12px] text-ink-faint">
                目录里还没有任何工具名可参照，请在下方直接输入。
              </p>
            ) : (
              <div className="grid max-h-[190px] grid-cols-2 gap-1 overflow-y-auto rounded-subtle border border-border/[0.08] bg-canvas p-2">
                {toolChoices.map((tool) => (
                  <label
                    key={tool}
                    className="flex cursor-pointer items-center gap-2 rounded-subtle px-1.5 py-1 font-mono text-[12px] text-ink-muted hover:bg-surface-raised hover:text-ink"
                  >
                    <input
                      type="checkbox"
                      checked={tools.includes(tool)}
                      onChange={() => toggleTool(tool)}
                      className="accent-accent"
                    />
                    <span className="truncate">{tool}</span>
                  </label>
                ))}
              </div>
            )}

            <div className="flex items-center gap-2">
              <input
                type="text"
                value={newTool}
                onChange={(e) => setNewTool(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    e.preventDefault()
                    addTool()
                  }
                }}
                placeholder="添加工具名（清单里没有的也能填）"
                className={FIELD_CLASS}
              />
              <button
                type="button"
                onClick={addTool}
                className="shrink-0 rounded-subtle border border-border/[0.1] px-3 py-2 text-[12.5px] font-medium text-ink-muted transition-colors hover:bg-surface-raised hover:text-ink"
              >
                添加
              </button>
            </div>
            <p className="text-[11.5px] text-ink-faint">
              后端不校验工具名：真正下发给子 Agent 时会按本 build 的工具注册表过滤。
            </p>
          </div>
        </div>

        <div className="flex flex-col gap-2 border-t border-border/[0.08] bg-surface-raised p-4">
          {formError && (
            <div className="flex items-start gap-2 rounded-subtle border border-danger/30 bg-danger/10 p-2.5 text-[12.5px] text-ink">
              <AlertCircle size={14} className="mt-0.5 shrink-0 text-danger" />
              <span>{formError}</span>
            </div>
          )}

          <div className="flex items-center justify-end gap-2">
            <button
              type="button"
              onClick={onClose}
              className="flex items-center gap-1 rounded-subtle border border-border/[0.1] px-3 py-2 text-[12.5px] font-medium text-ink-muted transition-colors hover:bg-surface hover:text-ink"
            >
              <X size={14} />
              <span>取消</span>
            </button>
            <button
              type="button"
              onClick={() => void handleSubmit()}
              disabled={busy}
              className="glass-accent-solid flex items-center gap-1.5 px-3.5 py-2 text-[12.5px] font-medium disabled:opacity-50"
            >
              {busy && <Loader2 size={13} className="animate-spin" />}
              <span>{editing ? '保存修改' : '创建专家'}</span>
            </button>
          </div>
        </div>
      </div>
    </div>
  )
}
