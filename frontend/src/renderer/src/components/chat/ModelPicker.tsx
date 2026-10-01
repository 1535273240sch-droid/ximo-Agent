import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { CSSProperties, KeyboardEvent as ReactKeyboardEvent } from 'react'
import { createPortal } from 'react-dom'
import { AlertCircle, Check, ChevronDown, Cpu, Loader2, RefreshCw, Search } from 'lucide-react'
import type { ModelListPayload } from '@shared/types'

/** localStorage 键：选择只在 ModelPicker 内落地，app-store 通过下面两个纯函数复用。 */
const MODEL_STORAGE_KEY = 'ximo.model'
/** 面板宽度（px）。定位与单测共用。 */
export const MODEL_PANEL_WIDTH = 300
/** 面板最大高度（px）。 */
const PANEL_MAX_HEIGHT = 360
/** 下方可用空间小于该值时强制向上展开，避免贴底时被窗口裁掉。 */
const FLIP_MIN_SPACE_BELOW = 260
/** 模型数量超过该阈值才显示搜索框。 */
const SEARCH_THRESHOLD = 5

/**
 * 读取上次选中的模型（持久化在 localStorage['ximo.model']）。
 *
 * 任何异常（隐私模式、非浏览器环境、配额报错）都静默降级为 undefined，
 * 不抛出、不阻塞渲染。
 */
export function loadPersistedModel(): string | undefined {
  try {
    const raw = window.localStorage.getItem(MODEL_STORAGE_KEY)
    const trimmed = raw?.trim()
    return trimmed ? trimmed : undefined
  } catch {
    return undefined
  }
}

/**
 * 持久化本次选择的模型；传入 undefined/空串表示「跟随全局默认」，会删除该键。
 * 写入失败不影响本次选择的生效，只是下次启动无法恢复。
 */
export function persistModel(m?: string): void {
  try {
    const trimmed = m?.trim()
    if (trimmed) window.localStorage.setItem(MODEL_STORAGE_KEY, trimmed)
    else window.localStorage.removeItem(MODEL_STORAGE_KEY)
  } catch {
    // 静默降级：localStorage 不可用时选择依然对本次会话生效。
  }
}

export interface PanelPosition {
  /** 是否向上展开。 */
  placeUp: boolean
  style: CSSProperties
}

/**
 * 计算 Portal 面板的 fixed 定位样式。
 *
 * 输入框固定在窗口底部，且祖先链上有 overflow-hidden（ChatView 根节点、
 * App.tsx 的 main），原来挂在按钮文档流里的 `absolute top-full` 会被整块
 * 裁掉，用户看到的等价于「点了没反应」。改为 createPortal 到 body + fixed
 * 定位：按按钮 getBoundingClientRect() 计算，下方空间不足时自动向上翻转。
 */
export function computePanelPosition(
  rect: { top: number; bottom: number; right: number },
  viewport: { width: number; height: number }
): PanelPosition {
  const spaceAbove = rect.top
  const spaceBelow = viewport.height - rect.bottom
  const placeUp = spaceAbove > spaceBelow || spaceBelow < FLIP_MIN_SPACE_BELOW
  // 预留 16px 边距；极窄窗口下保底 96px，避免 maxHeight 变成 0/负数让面板不可用。
  const rawAvailable = (placeUp ? spaceAbove : spaceBelow) - 16
  const maxHeight = Math.min(PANEL_MAX_HEIGHT, Math.max(rawAvailable, 96))
  // 左边界至少离窗口 8px；正常输入框在右下角，不会触发这个夹取。
  const left = Math.max(
    8,
    Math.min(rect.right - MODEL_PANEL_WIDTH, viewport.width - MODEL_PANEL_WIDTH - 8)
  )
  return {
    placeUp,
    style: {
      position: 'fixed',
      left,
      width: MODEL_PANEL_WIDTH,
      maxHeight,
      ...(placeUp
        ? { bottom: viewport.height - rect.top + 8 }
        : { top: rect.bottom + 8 })
    }
  }
}

interface ModelPickerProps {
  /** 当前选中的模型 ID；undefined 表示跟随设置页的全局默认配置。 */
  value?: string
  onChange: (model?: string) => void
}

/**
 * 首页输入框旁的「本次模型」选择器。
 *
 * 数据源复用设置页「获取模型」按钮背后的 window.ximo.listModels()，
 * 不另外发明获取模型列表的逻辑。每次展开时现拉一次，保证选项和
 * 设置页看到的一致；选择只对下一次提交生效。
 *
 * 面板走 Portal 渲染到 document.body，并用 fixed + 上下自动翻转定位，
 * 避免被输入框祖先的 overflow-hidden 裁掉。
 */
export function ModelPicker({ value, onChange }: ModelPickerProps): React.JSX.Element {
  const [open, setOpen] = useState(false)
  const [payload, setPayload] = useState<ModelListPayload | null>(null)
  const [loading, setLoading] = useState(false)
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(-1)
  const [position, setPosition] = useState<PanelPosition | null>(null)
  const rootRef = useRef<HTMLDivElement>(null)
  const buttonRef = useRef<HTMLButtonElement>(null)
  const panelRef = useRef<HTMLDivElement>(null)

  const fetchModels = async (): Promise<void> => {
    setLoading(true)
    try {
      setPayload(await window.ximo.listModels())
    } catch (err) {
      setPayload({ models: null, base_url: '', error: (err as Error).message })
    } finally {
      setLoading(false)
    }
  }

  const close = useCallback((): void => {
    setOpen(false)
    setQuery('')
    setActiveIndex(-1)
  }, [])

  const reposition = useCallback((): void => {
    const btn = buttonRef.current
    if (!btn) return
    setPosition(
      computePanelPosition(btn.getBoundingClientRect(), {
        width: window.innerWidth,
        height: window.innerHeight
      })
    )
  }, [])

  const toggleOpen = (): void => {
    if (open) {
      close()
      return
    }
    setQuery('')
    setActiveIndex(-1)
    setOpen(true)
    void fetchModels()
  }

  // 展开时先量一次位置，并在窗口 resize / 任意祖先滚动时重算（面板是 fixed，
  // 按钮会随滚动移动，重算比关闭体验更好）。
  useLayoutEffect(() => {
    if (!open) return
    reposition()
    const handleViewportChange = (): void => reposition()
    window.addEventListener('resize', handleViewportChange)
    document.addEventListener('scroll', handleViewportChange, true)
    return () => {
      window.removeEventListener('resize', handleViewportChange)
      document.removeEventListener('scroll', handleViewportChange, true)
    }
  }, [open, reposition])

  // 点外部或按 Esc 收起下拉。面板在 Portal 里，不属于 rootRef，必须单独判断，
  // 否则点面板内部也会被当成「外部点击」而误关。
  useEffect(() => {
    if (!open) return
    const handlePointerDown = (e: MouseEvent): void => {
      const target = e.target as Node
      if (rootRef.current?.contains(target)) return
      if (panelRef.current?.contains(target)) return
      close()
    }
    const handleKeyDown = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') close()
    }
    document.addEventListener('mousedown', handlePointerDown)
    document.addEventListener('keydown', handleKeyDown)
    return () => {
      document.removeEventListener('mousedown', handlePointerDown)
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [open, close])

  const models = payload?.models ?? []
  const showSearch = models.length > SEARCH_THRESHOLD
  const keyword = query.trim().toLowerCase()
  const visibleModels = keyword
    ? models.filter(
        (m) =>
          m.id.toLowerCase().includes(keyword) ||
          (m.owned_by ?? '').toLowerCase().includes(keyword)
      )
    : models
  // 只在「成功拿到非空模型列表」时才提示模型不在列表中，避免列表拉取失败时误报；
  // 命中该状态也不清空 value，用户的显式选择始终保留。
  const modelUnknown =
    Boolean(value) && !payload?.error && models.length > 0 && !models.some((m) => m.id === value)

  const selectModel = (id?: string): void => {
    onChange(id)
    persistModel(id)
    close()
  }

  // 模型不多时没有搜索框，焦点仍在触发按钮上；把面板本身设为可聚焦并在展开后
  // 主动聚焦，↑↓/Enter 才能冒泡到面板的 onKeyDown（搜索框存在时由 autoFocus
  // 接管，不抢焦点）。position 就绪前 Portal 还没挂载，此时 panelRef 为空。
  const panelReady = position !== null
  useEffect(() => {
    if (!open || showSearch || !panelReady) return
    panelRef.current?.focus()
  }, [open, showSearch, panelReady])

  const handlePanelKeyDown = (e: ReactKeyboardEvent<HTMLDivElement>): void => {
    if (e.key === 'ArrowDown') {
      if (visibleModels.length === 0) return
      e.preventDefault()
      setActiveIndex((i) => (i + 1) % visibleModels.length)
    } else if (e.key === 'ArrowUp') {
      if (visibleModels.length === 0) return
      e.preventDefault()
      setActiveIndex((i) => (i <= 0 ? visibleModels.length - 1 : i - 1))
    } else if (e.key === 'Enter') {
      const picked = activeIndex >= 0 ? visibleModels[activeIndex] : undefined
      if (!picked) return
      e.preventDefault()
      selectModel(picked.id)
    }
  }

  return (
    <div ref={rootRef} className="relative">
      <button
        ref={buttonRef}
        type="button"
        data-testid="model-picker-trigger"
        onClick={toggleOpen}
        aria-haspopup="listbox"
        aria-expanded={open}
        title={
          modelUnknown
            ? `该模型不在当前服务商列表中：${value}`
            : value
              ? `本次请求使用模型：${value}`
              : '选择本次请求使用的模型（不选则跟随设置页的全局配置）'
        }
        className={[
          'flex h-8 max-w-[180px] items-center gap-1.5 rounded-[6px] border px-2.5 text-[12px] transition-colors',
          modelUnknown
            ? 'border-warning/50 bg-warning/10 text-warning'
            : value
              ? 'border-accent/40 bg-accent/10 text-accent'
              : 'border-border/[0.1] text-ink-muted hover:text-ink'
        ].join(' ')}
      >
        {modelUnknown ? (
          <AlertCircle size={12} className="shrink-0" />
        ) : (
          <Cpu size={12} className="shrink-0" />
        )}
        <span className="truncate font-mono">{value || '默认模型'}</span>
        <ChevronDown
          size={12}
          className={`shrink-0 transition-transform ${open ? 'rotate-180' : ''}`}
        />
      </button>

      {open &&
        position &&
        createPortal(
          <div
            ref={panelRef}
            data-testid="model-picker-panel"
            data-place={position.placeUp ? 'up' : 'down'}
            tabIndex={-1}
            aria-label="模型选择面板"
            onKeyDown={handlePanelKeyDown}
            style={position.style}
            className="glass-raised animate-glass-in z-[1000] flex flex-col overflow-hidden p-1"
          >
            {/* 回到全局默认：清空本次选择，提交时不带 model 字段 */}
            <button
              type="button"
              onClick={() => selectModel(undefined)}
              className="flex w-full shrink-0 items-center justify-between rounded-subtle px-2.5 py-2 text-left text-[12.5px] text-ink transition-colors hover:bg-surface"
            >
              <span>跟随全局默认配置</span>
              {!value && <Check size={13} className="shrink-0 text-accent" />}
            </button>

            {showSearch && (
              <div className="shrink-0 px-1 pb-1 pt-0.5">
                <div className="flex items-center gap-1.5 rounded-subtle border border-border/[0.1] px-2">
                  <Search size={12} className="shrink-0 text-ink-faint" />
                  <input
                    autoFocus
                    value={query}
                    aria-label="搜索模型"
                    placeholder="搜索模型…"
                    onChange={(e) => {
                      setQuery(e.target.value)
                      setActiveIndex(-1)
                    }}
                    className="h-7 w-full bg-transparent text-[12.5px] text-ink outline-none placeholder:text-ink-faint"
                  />
                </div>
              </div>
            )}

            <div className="mx-2.5 my-1 h-px shrink-0 bg-border/[0.07]" />

            {loading ? (
              <div className="flex items-center gap-2 px-2.5 py-2.5 text-[12px] text-ink-muted">
                <Loader2 size={13} className="animate-spin" />
                <span>正在获取模型列表…</span>
              </div>
            ) : payload?.error ? (
              <div className="flex items-start gap-2 px-2.5 py-2.5 text-[12px] leading-relaxed text-warning">
                <AlertCircle size={13} className="mt-0.5 shrink-0" />
                <span>获取模型失败：{payload.error}</span>
              </div>
            ) : models.length === 0 ? (
              <div className="px-2.5 py-2.5 text-[12px] text-ink-muted">
                暂无模型。请先到设置页配置服务商并保存密钥。
              </div>
            ) : visibleModels.length === 0 ? (
              <div className="px-2.5 py-2.5 text-[12px] text-ink-muted">
                没有匹配「{query.trim()}」的模型。
              </div>
            ) : (
              <div role="listbox" aria-label="模型列表" className="min-h-0 flex-1 overflow-y-auto">
                {visibleModels.map((m, index) => (
                  <button
                    key={m.id}
                    type="button"
                    role="option"
                    aria-selected={value === m.id}
                    data-testid="model-option"
                    onMouseEnter={() => setActiveIndex(index)}
                    onClick={() => selectModel(m.id)}
                    className={[
                      'flex w-full items-center justify-between rounded-subtle px-2.5 py-2 text-left transition-colors hover:bg-surface',
                      activeIndex === index ? 'bg-surface' : ''
                    ].join(' ')}
                    title={m.id}
                  >
                    <span className="truncate font-mono text-[12.5px] text-ink">{m.id}</span>
                    <span className="flex shrink-0 items-center gap-1.5 pl-2">
                      {m.owned_by && (
                        <span className="text-[11px] text-ink-faint">{m.owned_by}</span>
                      )}
                      {value === m.id && <Check size={13} className="shrink-0 text-accent" />}
                    </span>
                  </button>
                ))}
              </div>
            )}

            <div className="mx-2.5 my-1 h-px shrink-0 bg-border/[0.07]" />

            <button
              type="button"
              onClick={() => void fetchModels()}
              disabled={loading}
              className="flex w-full shrink-0 items-center gap-1.5 rounded-subtle px-2.5 py-2 text-left text-[12px] text-ink-muted transition-colors hover:text-ink disabled:opacity-50"
            >
              {loading ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />}
              <span>重新获取</span>
            </button>
          </div>,
          document.body
        )}
    </div>
  )
}
