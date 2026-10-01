import { useRef, useState } from 'react'
import { AlertTriangle, Download, Loader2, Moon, Sparkles, Upload } from 'lucide-react'
import { DEFAULT_MEMORY_SETTINGS, type MemoryExport, type MemorySettings, type MemoryStats } from '@shared/types'
import { parseMemoryExport } from '../../store/memory-store'

/** 设置持久化键。 */
export const MEMORY_SETTINGS_KEY = 'ximo.memory.settings.v1'

/** 清空记忆要求的确认字面量，与后端 ipcapi 的 memoryClearConfirm 一致。 */
export const MEMORY_CLEAR_CONFIRM = 'DELETE_ALL'

/** 读取本地记忆设置（不可用时静默降级为默认值）。 */
export function loadMemorySettings(): MemorySettings {
  try {
    const raw = localStorage.getItem(MEMORY_SETTINGS_KEY)
    if (!raw) return { ...DEFAULT_MEMORY_SETTINGS }
    const parsed = JSON.parse(raw) as Partial<MemorySettings>
    return {
      enabled: typeof parsed.enabled === 'boolean' ? parsed.enabled : DEFAULT_MEMORY_SETTINGS.enabled,
      autoExtract:
        typeof parsed.autoExtract === 'boolean'
          ? parsed.autoExtract
          : DEFAULT_MEMORY_SETTINGS.autoExtract,
      autoConsolidate:
        typeof parsed.autoConsolidate === 'boolean'
          ? parsed.autoConsolidate
          : DEFAULT_MEMORY_SETTINGS.autoConsolidate,
      embeddingModel:
        typeof parsed.embeddingModel === 'string'
          ? parsed.embeddingModel
          : DEFAULT_MEMORY_SETTINGS.embeddingModel
    }
  } catch {
    return { ...DEFAULT_MEMORY_SETTINGS }
  }
}

/** 保存本地记忆设置。 */
export function saveMemorySettings(s: MemorySettings): void {
  try {
    localStorage.setItem(MEMORY_SETTINGS_KEY, JSON.stringify(s))
  } catch {
    // 存储不可用：设置只在本次会话内有效，不影响记忆功能本身。
  }
}

/**
 * 设置面板（审核文档 4.9 第 4 条）。
 *
 * 四项设置 + 导出/导入 + 清空全部记忆。
 *
 * 「清空全部记忆」刻意做成两段式：先展开一个输入框，用户必须手打 DELETE_ALL
 * 才能点下按钮。后端也要求同一个字面量——这不是前端的花架子，而是让"确认"这个
 * 动作必须由人完成（IPC 载荷是可以被脚本拼出来的）。
 */
interface MemorySettingsPanelProps {
  stats: MemoryStats | null
  onConsolidate: () => Promise<void>
  onExport: () => Promise<MemoryExport>
  onImport: (doc: MemoryExport) => Promise<void>
  onClear: (confirm: string) => Promise<void>
  onSettingsChange?: (s: MemorySettings) => void
}

export function MemorySettingsPanel({
  stats,
  onConsolidate,
  onExport,
  onImport,
  onClear,
  onSettingsChange
}: MemorySettingsPanelProps): React.JSX.Element {
  const [settings, setSettings] = useState<MemorySettings>(() => loadMemorySettings())
  const [busy, setBusy] = useState<string | null>(null)
  const [message, setMessage] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [clearOpen, setClearOpen] = useState(false)
  const [clearText, setClearText] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)

  const update = (patch: Partial<MemorySettings>): void => {
    const next = { ...settings, ...patch }
    setSettings(next)
    saveMemorySettings(next)
    onSettingsChange?.(next)
  }

  const run = (key: string, fn: () => Promise<void>, ok: string): void => {
    setBusy(key)
    setError(null)
    setMessage(null)
    void fn()
      .then(() => setMessage(ok))
      .catch((err: unknown) => setError(err instanceof Error ? err.message : String(err)))
      .finally(() => setBusy(null))
  }

  const handleExport = (): void => {
    run(
      'export',
      async () => {
        const doc = await onExport()
        const text = JSON.stringify(doc, null, 2)
        const blob = new Blob([text], { type: 'application/json' })
        const url = URL.createObjectURL(blob)
        const a = document.createElement('a')
        a.href = url
        a.download = `ximo-memory-${new Date().toISOString().slice(0, 10)}.json`
        document.body.appendChild(a)
        a.click()
        a.remove()
        URL.revokeObjectURL(url)
      },
      '已导出记忆文件'
    )
  }

  const handleImportFile = (file: File): void => {
    run(
      'import',
      async () => {
        const text = await file.text()
        const doc = parseMemoryExport(JSON.parse(text))
        await onImport(doc)
      },
      '已导入记忆文件'
    )
  }

  const clearReady = clearText.trim() === MEMORY_CLEAR_CONFIRM

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        {/* 开关组 */}
        <section className="mb-5">
          <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            记忆开关
          </h3>
          <div className="flex flex-col gap-2">
            <Toggle
              label="启用长期记忆"
              hint="关闭后不再从对话中写入，也不再召回；已有记忆不会被删除。"
              checked={settings.enabled}
              onChange={(v) => update({ enabled: v })}
            />
            <Toggle
              label="允许自动抽取"
              hint="每轮对话后自动把值得记住的事实写进记忆图。关闭后只能手动添加。"
              checked={settings.autoExtract}
              onChange={(v) => update({ autoExtract: v })}
              disabled={!settings.enabled}
            />
            <Toggle
              label="允许后台整理"
              hint="空闲时合并重复事实、聚合主题、归档长期未用的节点。"
              checked={settings.autoConsolidate}
              onChange={(v) => update({ autoConsolidate: v })}
              disabled={!settings.enabled}
            />
          </div>
        </section>

        {/* 嵌入模型 */}
        <section className="mb-5">
          <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            嵌入模型（可选）
          </h3>
          <input
            type="text"
            aria-label="嵌入模型"
            value={settings.embeddingModel}
            onChange={(e) => update({ embeddingModel: e.target.value })}
            placeholder="例如 text-embedding-3-small；留空表示不用"
            className="glass-inset w-full px-2.5 py-1.5 text-[12.5px] text-ink outline-none placeholder:text-ink-faint"
          />
          <p className="mt-1 text-[11.5px] leading-relaxed text-ink-faint">
            未配置时同义改写命中率较低：召回只能靠词法匹配（BM25）与实体链接，
            换个说法问同一件事可能想不起来。
          </p>
        </section>

        {/* 维护操作 */}
        <section className="mb-5">
          <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            维护
          </h3>
          <div className="flex flex-wrap items-center gap-2">
            <button
              type="button"
              onClick={() => run('consolidate', onConsolidate, '整理完成')}
              disabled={busy !== null}
              className="flex items-center gap-1.5 rounded-[6px] border border-border/[0.12] px-2.5 py-1.5 text-[12.5px] text-ink-muted transition-colors hover:bg-surface-raised hover:text-ink disabled:opacity-40"
            >
              {busy === 'consolidate' ? (
                <Loader2 size={13} className="animate-spin" />
              ) : (
                <Moon size={13} />
              )}
              立即整理
            </button>
            <button
              type="button"
              onClick={handleExport}
              disabled={busy !== null}
              className="flex items-center gap-1.5 rounded-[6px] border border-border/[0.12] px-2.5 py-1.5 text-[12.5px] text-ink-muted transition-colors hover:bg-surface-raised hover:text-ink disabled:opacity-40"
            >
              {busy === 'export' ? <Loader2 size={13} className="animate-spin" /> : <Download size={13} />}
              导出 JSON
            </button>
            <button
              type="button"
              onClick={() => fileRef.current?.click()}
              disabled={busy !== null}
              className="flex items-center gap-1.5 rounded-[6px] border border-border/[0.12] px-2.5 py-1.5 text-[12.5px] text-ink-muted transition-colors hover:bg-surface-raised hover:text-ink disabled:opacity-40"
            >
              {busy === 'import' ? <Loader2 size={13} className="animate-spin" /> : <Upload size={13} />}
              导入 JSON
            </button>
            <input
              ref={fileRef}
              type="file"
              accept="application/json,.json"
              aria-label="选择记忆文件"
              className="hidden"
              onChange={(e) => {
                const f = e.target.files?.[0]
                // 清空 value：否则同一个文件连续选两次不会触发 change。
                e.target.value = ''
                if (f) handleImportFile(f)
              }}
            />
          </div>
          <p className="mt-1 text-[11.5px] leading-relaxed text-ink-faint">
            导入按内容哈希幂等合并，不覆盖已有内容；两端都存在的节点才会连边。
          </p>
        </section>

        {/* 统计 */}
        {stats && (
          <section className="mb-5">
            <h3 className="mb-2 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
              当前状态
            </h3>
            <div className="grid grid-cols-2 gap-x-4 gap-y-1 text-[12px] text-ink-muted sm:grid-cols-3">
              <span>后端 <span className="font-mono text-ink">{stats.backend || '—'}</span></span>
              <span>节点 <span className="font-mono text-ink">{stats.nodes}</span></span>
              <span>边 <span className="font-mono text-ink">{stats.edges}</span></span>
              <span>召回 <span className="font-mono text-ink">{stats.recall_calls}</span> 次</span>
              <span>整理 <span className="font-mono text-ink">{stats.consolidations}</span> 次</span>
              <span>归档 <span className="font-mono text-ink">{stats.archived_nodes}</span> 条</span>
            </div>
            {stats.last_error && (
              <p className="mt-1.5 rounded-[6px] border border-warning/30 bg-warning/10 px-2 py-1 text-[11.5px] text-warning">
                最近一次错误：{stats.last_error}
              </p>
            )}
          </section>
        )}

        {/* 危险区 */}
        <section className="mb-4 rounded-[8px] border border-danger/25 p-3">
          <h3 className="mb-1 flex items-center gap-1.5 text-[12px] font-semibold text-danger">
            <AlertTriangle size={13} />
            清空全部记忆
          </h3>
          <p className="mb-2 text-[11.5px] leading-relaxed text-ink-muted">
            删除该用户的全部节点、连接与召回记录。不可撤销，也没有回收站。
          </p>
          {!clearOpen ? (
            <button
              type="button"
              onClick={() => setClearOpen(true)}
              className="rounded-[6px] border border-danger/40 px-2.5 py-1 text-[12px] text-danger transition-colors hover:bg-danger/10"
            >
              我要清空…
            </button>
          ) : (
            <div className="flex flex-col gap-2">
              <label className="text-[11.5px] text-ink-muted" htmlFor="memory-clear-confirm">
                输入 <span className="font-mono text-danger">{MEMORY_CLEAR_CONFIRM}</span> 以确认
              </label>
              <div className="flex items-center gap-2">
                <input
                  id="memory-clear-confirm"
                  aria-label="清空确认字面量"
                  value={clearText}
                  onChange={(e) => setClearText(e.target.value)}
                  placeholder={MEMORY_CLEAR_CONFIRM}
                  className="glass-inset flex-1 px-2 py-1 font-mono text-[12.5px] text-ink outline-none"
                />
                <button
                  type="button"
                  disabled={!clearReady || busy !== null}
                  onClick={() => {
                    // 成功后才收起确认框；失败时保留输入，让用户可以直接重试。
                    run(
                      'clear',
                      async () => {
                        await onClear(MEMORY_CLEAR_CONFIRM)
                        setClearText('')
                        setClearOpen(false)
                      },
                      '已清空全部记忆'
                    )
                  }}
                  className="rounded-[6px] bg-danger/90 px-3 py-1 text-[12px] font-medium text-white transition-opacity disabled:opacity-30"
                >
                  清空
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setClearOpen(false)
                    setClearText('')
                  }}
                  className="text-[11.5px] text-ink-faint hover:text-ink"
                >
                  取消
                </button>
              </div>
            </div>
          )}
        </section>

        {message && (
          <p className="mb-2 flex items-center gap-1.5 text-[12px] text-success">
            <Sparkles size={12} />
            {message}
          </p>
        )}
        {error && (
          <p className="mb-2 rounded-[6px] border border-danger/30 bg-danger/10 px-2 py-1 text-[12px] text-danger">
            {error}
          </p>
        )}
      </div>
    </div>
  )
}

function Toggle({
  label,
  hint,
  checked,
  onChange,
  disabled
}: {
  label: string
  hint: string
  checked: boolean
  onChange: (v: boolean) => void
  disabled?: boolean
}): React.JSX.Element {
  return (
    <label
      className={[
        'flex cursor-pointer items-start gap-2.5 rounded-[6px] px-1 py-1',
        disabled ? 'cursor-not-allowed opacity-50' : ''
      ].join(' ')}
    >
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-0.5 accent-[rgb(var(--c-accent))]"
      />
      <span className="flex flex-col">
        <span className="text-[13px] text-ink">{label}</span>
        <span className="text-[11.5px] leading-relaxed text-ink-faint">{hint}</span>
      </span>
    </label>
  )
}
