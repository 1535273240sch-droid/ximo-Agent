import { useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { Check, Sparkles, UserCheck, Search, X, Bot } from 'lucide-react'
import {
  DIVISION_LABELS,
  EXPERTS,
  type Expert
} from './experts-data'
import { ExpertEmoji } from './ExpertCard'

export interface ExpertQuickPickerProps {
  /** 当前输入框文字，用于动态推荐匹配 */
  inputText: string
  /** 当前选中的专家 ID */
  selectedExpertId?: string
  /** 选中专家（传入 undefined 表示取消选择） */
  onSelect: (expert?: Expert) => void
  /** 关闭弹窗 */
  onClose: () => void
}

/** 候选席位键盘快捷键标签 */
const SHORTCUT_KEYS = ['A', 'B', 'C', 'D'] as const

/**
 * 根据输入框内容做关键词加权匹配推荐。
 * 无输入或无匹配时返回通用的全栈高级工程师。
 */
function getRecommendedExpert(text: string): Expert {
  const q = text.trim().toLowerCase()
  if (!q) {
    return (
      EXPERTS.find((e) => e.id === 'engineering-senior-developer') ??
      EXPERTS[0]
    )
  }

  let bestExpert = EXPERTS[0]
  let bestScore = -1

  for (const exp of EXPERTS) {
    let score = 0
    const name = exp.name.toLowerCase()
    const desc = exp.description.toLowerCase()
    const div = exp.division.toLowerCase()
    const divLabel = (DIVISION_LABELS[exp.division] ?? '').toLowerCase()
    const vibe = (exp.vibe ?? '').toLowerCase()

    // 常见关键词打分
    for (const token of q.split(/[\s,，.。!！?？;；、]+/)) {
      if (!token) continue
      if (name.includes(token)) score += 10
      if (divLabel.includes(token)) score += 6
      if (div.includes(token)) score += 5
      if (desc.includes(token)) score += 3
      if (vibe.includes(token)) score += 2
    }

    if (score > bestScore) {
      bestScore = score
      bestExpert = exp
    }
  }

  if (bestScore <= 0) {
    return (
      EXPERTS.find((e) => e.id === 'engineering-senior-developer') ??
      EXPERTS[0]
    )
  }
  return bestExpert
}

/**
 * 专家快捷选择弹窗 (Popover)。
 *
 * 锚定在输入框上方：
 * - 顶部搜索框：自由检索全部 60 位覆盖各部门的代表专家。
 * - 推荐区域：根据当前输入文字实时动态匹配 1 位最匹配的专家。
 * - 候选区域：3~4 个候选专家，支持 A/B/C/D 快捷键一键秒选。
 * - 样式完全跟随 5 套主题 CSS 变量（bg-surface / text-ink / border 等）。
 */
export function ExpertQuickPicker({
  inputText,
  selectedExpertId,
  onSelect,
  onClose
}: ExpertQuickPickerProps): React.JSX.Element {
  const [searchQuery, setSearchQuery] = useState('')
  const rootRef = useRef<HTMLDivElement>(null)
  const searchInputRef = useRef<HTMLInputElement>(null)

  // 1. 动态推荐位专家（响应当前输入内容）
  const recommended = useMemo(() => {
    return getRecommendedExpert(inputText)
  }, [inputText])

  // 2. 搜索过滤列表
  const searchResults = useMemo(() => {
    const q = searchQuery.trim().toLowerCase()
    if (!q) return []
    return EXPERTS.filter((e) => {
      const divLabel = (DIVISION_LABELS[e.division] ?? '').toLowerCase()
      return (
        e.name.toLowerCase().includes(q) ||
        e.description.toLowerCase().includes(q) ||
        e.division.toLowerCase().includes(q) ||
        divLabel.includes(q)
      )
    })
  }, [searchQuery])

  // 3. 候选专家（3~4 位）
  const candidateExperts = useMemo(() => {
    if (searchQuery.trim()) {
      // 搜索模式下取前 4 个搜索结果
      return searchResults.slice(0, 4)
    }
    // 默认展示与推荐不同的常用高频专家
    const presetIds = [
      'engineering-frontend-developer',
      'design-ui-designer',
      'product-manager',
      'engineering-code-reviewer'
    ]
    const list = presetIds
      .map((id) => EXPERTS.find((e) => e.id === id))
      .filter((e): e is Expert => Boolean(e && e.id !== recommended.id))

    // 如果被推荐去重后少于 4 个，从前面补齐
    for (const exp of EXPERTS) {
      if (list.length >= 4) break
      if (exp.id !== recommended.id && !list.some((item) => item.id === exp.id)) {
        list.push(exp)
      }
    }
    return list.slice(0, 4)
  }, [searchQuery, searchResults, recommended.id])

  // 外部点击与键盘 Esc 监听
  useEffect(() => {
    const handlePointerDown = (e: MouseEvent): void => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        onClose()
      }
    }
    const handleKeyDown = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') {
        onClose()
      }
    }
    document.addEventListener('mousedown', handlePointerDown)
    document.addEventListener('keydown', handleKeyDown)
    return () => {
      document.removeEventListener('mousedown', handlePointerDown)
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [onClose])

  // 快捷键 A/B/C/D 快捷选取候选专家
  const handleGlobalKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>): void => {
    // 如果用户正在输入搜索词且按的是普通英文字符，不拦截用户打字；
    // 但如果按 Alt + A/B/C/D 或者当前焦点不在 input 框，则秒选
    const keyUpper = e.key.toUpperCase()
    const index = SHORTCUT_KEYS.indexOf(keyUpper as (typeof SHORTCUT_KEYS)[number])
    const isInputFocused = document.activeElement === searchInputRef.current

    if (index >= 0 && index < candidateExperts.length) {
      if (!isInputFocused || e.altKey) {
        e.preventDefault()
        const chosen = candidateExperts[index]
        if (chosen) {
          onSelect(chosen)
          onClose()
        }
      }
    }
  }

  return (
    <div
      ref={rootRef}
      onKeyDown={handleGlobalKeyDown}
      className="glass-raised animate-glass-in absolute bottom-full left-0 z-30 mb-2 flex w-[340px] flex-col overflow-hidden rounded-[10px] border border-border/[0.14] bg-surface-raised p-0 shadow-card select-none"
      role="dialog"
      aria-label="专家选择器"
    >
      {/* 顶部标题栏与搜索 */}
      <div className="flex flex-col gap-2 border-b border-border/[0.08] bg-surface p-2.5">
        <div className="flex items-center justify-between px-0.5">
          <div className="flex items-center gap-1.5 text-[12px] font-semibold text-ink">
            <Bot size={13} className="text-accent" />
            <span>AI 专家智能调度</span>
          </div>
          {selectedExpertId && (
            <button
              type="button"
              onClick={() => {
                onSelect(undefined)
                onClose()
              }}
              className="text-[11.5px] text-ink-muted hover:text-danger transition-colors"
            >
              清除已选
            </button>
          )}
        </div>

        {/* 自由检索输入框 */}
        <div className="glass-inset flex items-center gap-2 px-2.5 py-1 text-[12.5px]">
          <Search size={13} className="shrink-0 text-ink-muted" />
          <input
            ref={searchInputRef}
            type="text"
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            placeholder="搜索专家名称、部门或技能…"
            className="w-full bg-transparent text-[12px] text-ink outline-none placeholder:text-ink-faint font-ui"
          />
          {searchQuery && (
            <button
              type="button"
              onClick={() => setSearchQuery('')}
              className="text-ink-muted hover:text-ink transition-colors"
              title="清除搜索"
            >
              <X size={13} />
            </button>
          )}
        </div>
      </div>

      <div className="max-h-[320px] overflow-y-auto p-2 flex flex-col gap-2">
        {/* 搜索模式下的结果 */}
        {searchQuery.trim() ? (
          <div>
            <div className="px-1.5 pb-1 text-[11px] font-medium uppercase tracking-wider text-ink-faint">
              搜索结果 ({searchResults.length})
            </div>
            {searchResults.length === 0 ? (
              <div className="py-6 text-center text-[12px] text-ink-muted">
                未找到匹配的专家
              </div>
            ) : (
              <div className="flex flex-col gap-1">
                {searchResults.map((exp, idx) => {
                  const isSelected = exp.id === selectedExpertId
                  const shortcut = idx < SHORTCUT_KEYS.length ? SHORTCUT_KEYS[idx] : null
                  return (
                    <button
                      key={exp.id}
                      type="button"
                      onClick={() => {
                        onSelect(exp)
                        onClose()
                      }}
                      className={`flex w-full items-center gap-2.5 rounded-subtle p-2 text-left transition-colors ${
                        isSelected
                          ? 'border border-accent/40 bg-accent/15 text-ink'
                          : 'hover:bg-surface border border-transparent'
                      }`}
                    >
                      <ExpertEmoji emoji={exp.emoji} color={exp.color} size="sm" />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-1.5">
                          <span className="truncate text-[13px] font-medium text-ink">
                            {exp.name}
                          </span>
                          <span className="text-[11px] text-ink-faint">
                            · {DIVISION_LABELS[exp.division] ?? exp.division}
                          </span>
                        </div>
                        <p className="truncate text-[11.5px] text-ink-muted">
                          {exp.description}
                        </p>
                      </div>
                      {shortcut && (
                        <span className="shrink-0 rounded border border-border/[0.1] bg-canvas px-1.5 py-0.5 font-mono text-[10.5px] text-ink-faint">
                          {shortcut}
                        </span>
                      )}
                      {isSelected && <Check size={14} className="shrink-0 text-accent" />}
                    </button>
                  )
                })}
              </div>
            )}
          </div>
        ) : (
          <>
            {/* 1. 推荐区域：根据输入框打的文字做关键词匹配 */}
            <div className="flex flex-col gap-1">
              <div className="flex items-center justify-between px-1.5 text-[11px] font-medium text-ink-muted">
                <span className="flex items-center gap-1 text-accent">
                  <Sparkles size={11} />
                  <span>智能推荐</span>
                </span>
                <span className="text-[10.5px] text-ink-faint">
                  {inputText.trim() ? '依据输入分析' : '默认推荐'}
                </span>
              </div>

              <button
                type="button"
                onClick={() => {
                  onSelect(recommended)
                  onClose()
                }}
                className={`flex w-full items-start gap-2.5 rounded-subtle border p-2 text-left transition-colors ${
                  recommended.id === selectedExpertId
                    ? 'border-accent/40 bg-accent/15'
                    : 'border-accent/20 bg-accent/[0.04] hover:bg-accent/[0.08]'
                }`}
              >
                <ExpertEmoji emoji={recommended.emoji} color={recommended.color} size="sm" />
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-1.5">
                    <span className="truncate text-[13px] font-semibold text-ink">
                      {recommended.name}
                    </span>
                    <span className="rounded bg-accent/15 px-1.5 py-0.2 font-mono text-[10.5px] font-medium text-accent">
                      {DIVISION_LABELS[recommended.division] ?? recommended.division}
                    </span>
                  </div>
                  <p className="mt-0.5 line-clamp-2 text-[11.5px] leading-relaxed text-ink-muted">
                    {recommended.description}
                  </p>
                </div>
                {recommended.id === selectedExpertId && (
                  <Check size={14} className="mt-1 shrink-0 text-accent" />
                )}
              </button>
            </div>

            {/* 2. 候选区域：3~4 个候选专家（配 A/B/C/D 快捷键） */}
            <div className="flex flex-col gap-1 pt-1">
              <div className="flex items-center justify-between px-1.5 text-[11px] font-medium text-ink-faint">
                <span>常用候选专家</span>
                <span className="font-mono text-[10px]">快捷键 A-D</span>
              </div>

              <div className="flex flex-col gap-1">
                {candidateExperts.map((exp, idx) => {
                  const isSelected = exp.id === selectedExpertId
                  const shortcut = SHORTCUT_KEYS[idx]
                  return (
                    <button
                      key={exp.id}
                      type="button"
                      onClick={() => {
                        onSelect(exp)
                        onClose()
                      }}
                      className={`flex w-full items-center gap-2.5 rounded-subtle p-2 text-left transition-colors ${
                        isSelected
                          ? 'border border-accent/40 bg-accent/15'
                          : 'hover:bg-surface border border-transparent'
                      }`}
                    >
                      <ExpertEmoji emoji={exp.emoji} color={exp.color} size="sm" />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center gap-1.5">
                          <span className="truncate text-[12.5px] font-medium text-ink">
                            {exp.name}
                          </span>
                          <span className="text-[11px] text-ink-faint">
                            · {DIVISION_LABELS[exp.division] ?? exp.division}
                          </span>
                        </div>
                        <p className="truncate text-[11.5px] text-ink-muted">
                          {exp.description}
                        </p>
                      </div>
                      <span
                        className="shrink-0 rounded border border-border/[0.1] bg-canvas px-1.5 py-0.5 font-mono text-[10.5px] font-medium text-ink-faint shadow-subtle"
                        title={`按 ${shortcut} 快捷选中`}
                      >
                        {shortcut}
                      </span>
                      {isSelected && <Check size={14} className="shrink-0 text-accent" />}
                    </button>
                  )
                })}
              </div>
            </div>
          </>
        )}
      </div>

      {/* 底部当前状态栏 */}
      <div className="flex items-center justify-between border-t border-border/[0.08] bg-surface px-3 py-2 text-[11.5px] text-ink-muted">
        <span className="flex items-center gap-1.5">
          <UserCheck size={12} className="text-accent" />
          <span>
            {selectedExpertId
              ? `已选绑定：${EXPERTS.find((e) => e.id === selectedExpertId)?.name ?? selectedExpertId}`
              : '未绑定专家（主模型自主判断）'}
          </span>
        </span>
        {selectedExpertId && (
          <button
            type="button"
            onClick={() => {
              onSelect(undefined)
              onClose()
            }}
            className="text-ink-faint hover:text-danger transition-colors font-medium"
          >
            取消
          </button>
        )}
      </div>
    </div>
  )
}
