import { useEffect, useRef, useState } from 'react'
import { AlertCircle, Check, ChevronDown, Cpu, Loader2, RefreshCw } from 'lucide-react'
import type { ModelListPayload } from '@shared/types'

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
 */
export function ModelPicker({ value, onChange }: ModelPickerProps): React.JSX.Element {
  const [open, setOpen] = useState(false)
  const [payload, setPayload] = useState<ModelListPayload | null>(null)
  const [loading, setLoading] = useState(false)
  const rootRef = useRef<HTMLDivElement>(null)

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

  const toggleOpen = (): void => {
    const next = !open
    setOpen(next)
    if (next) void fetchModels()
  }

  // 点外部或按 Esc 收起下拉。
  useEffect(() => {
    if (!open) return
    const handlePointerDown = (e: MouseEvent): void => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) setOpen(false)
    }
    const handleKeyDown = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', handlePointerDown)
    document.addEventListener('keydown', handleKeyDown)
    return () => {
      document.removeEventListener('mousedown', handlePointerDown)
      document.removeEventListener('keydown', handleKeyDown)
    }
  }, [open])

  const models = payload?.models ?? []

  return (
    <div ref={rootRef} className="relative">
      <button
        type="button"
        onClick={toggleOpen}
        title={
          value
            ? `本次请求使用模型：${value}`
            : '选择本次请求使用的模型（不选则跟随设置页的全局配置）'
        }
        className={[
          'flex h-8 max-w-[180px] items-center gap-1.5 rounded-[6px] border px-2.5 text-[12px] transition-colors',
          value
            ? 'border-accent/40 bg-accent/10 text-accent'
            : 'border-border/[0.1] text-ink-muted hover:text-ink'
        ].join(' ')}
      >
        <Cpu size={12} className="shrink-0" />
        <span className="truncate font-mono">{value || '默认模型'}</span>
        <ChevronDown
          size={12}
          className={`shrink-0 transition-transform ${open ? 'rotate-180' : ''}`}
        />
      </button>

      {open && (
        <div className="glass-raised animate-glass-in absolute right-0 top-full z-20 mt-1.5 w-[260px] p-1">
          {/* 回到全局默认：清空本次选择，提交时不带 model 字段 */}
          <button
            type="button"
            onClick={() => {
              onChange(undefined)
              setOpen(false)
            }}
            className="flex w-full items-center justify-between rounded-subtle px-2.5 py-2 text-left text-[12.5px] text-ink transition-colors hover:bg-surface"
          >
            <span>跟随全局默认配置</span>
            {!value && <Check size={13} className="shrink-0 text-accent" />}
          </button>

          <div className="mx-2.5 my-1 h-px bg-border/[0.07]" />

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
          ) : (
            <div className="max-h-[240px] overflow-y-auto">
              {models.map((m) => (
                <button
                  key={m.id}
                  type="button"
                  onClick={() => {
                    onChange(m.id)
                    setOpen(false)
                  }}
                  className="flex w-full items-center justify-between rounded-subtle px-2.5 py-2 text-left transition-colors hover:bg-surface"
                  title={m.id}
                >
                  <span className="truncate font-mono text-[12.5px] text-ink">{m.id}</span>
                  <span className="flex shrink-0 items-center gap-1.5 pl-2">
                    {m.owned_by && <span className="text-[11px] text-ink-faint">{m.owned_by}</span>}
                    {value === m.id && <Check size={13} className="shrink-0 text-accent" />}
                  </span>
                </button>
              ))}
            </div>
          )}

          <div className="mx-2.5 my-1 h-px bg-border/[0.07]" />

          <button
            type="button"
            onClick={() => void fetchModels()}
            disabled={loading}
            className="flex w-full items-center gap-1.5 rounded-subtle px-2.5 py-2 text-left text-[12px] text-ink-muted transition-colors hover:text-ink disabled:opacity-50"
          >
            {loading ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />}
            <span>重新获取</span>
          </button>
        </div>
      )}
    </div>
  )
}
