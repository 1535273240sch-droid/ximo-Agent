import { useCallback, useEffect, useState } from 'react'
import {
  AlertCircle,
  ArrowDown,
  ArrowUp,
  Bot,
  Loader2,
  Plus,
  Save,
  Sparkles,
  Trash2
} from 'lucide-react'
import { useStore } from '../../store/app-store'
import { DIVISIONS, DIVISION_LABELS } from '../experts/experts-data'
import type { ModelListPayload, ProviderEntryPayload, RuntimeSettingsPayload } from '@shared/types'

/**
 * 子代理模型分配面板（任务 5）。
 *
 * 管两件事，都落 config.json：
 *  1. 候选服务商池（providers）：主服务商恒在首位，其后是失败转移的候选，
 *     顺序即优先级 —— 某个候选 429/断线时按这个顺序换下一个。
 *  2. 候选模型分配（sub_agent）：全局池 + 按专家分类覆盖，专家 ID 级的
 *     精确覆盖由后端配置结构支持，这里先放开最常用的分类维度。
 *
 * 保存前重新拉一次配置再做合并：设置页其它面板（服务商凭据等）共享同一份
 * payload，先刷新可避免把用户在别处刚保存的改动用旧值冲掉。
 *
 * 「自动获取候选」：一键向主服务商拉取模型列表，勾选后批量生成候选行，
 * 免去手动复制粘贴模型名；同一模型已存在时自动跳过。
 */
export function SubAgentModelPanel(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const isBackendReady = backend.kind === 'ready'

  const [settings, setSettings] = useState<RuntimeSettingsPayload | null>(null)
  const [providers, setProviders] = useState<ProviderEntryPayload[]>([])
  const [pool, setPool] = useState<string[]>([])
  const [byDivision, setByDivision] = useState<Record<string, string[]>>({})
  const [saving, setSaving] = useState(false)
  const [feedback, setFeedback] = useState<{ type: 'success' | 'error'; msg: string } | null>(null)

  // 自动获取候选：拉取 → 勾选 → 批量追加为候选行。
  const [fetchingModels, setFetchingModels] = useState(false)
  const [modelPick, setModelPick] = useState<ModelListPayload | null>(null)
  const [picked, setPicked] = useState<Record<string, boolean>>({})

  const refresh = useCallback(async (): Promise<void> => {
    if (!isBackendReady) return
    try {
      const s = await window.ximo.getSettings()
      setSettings(s)
      setProviders(s.providers ?? [])
      setPool(s.sub_agent?.pool ?? [])
      setByDivision(s.sub_agent?.by_division ?? {})
      setFeedback(null)
    } catch (err) {
      setFeedback({ type: 'error', msg: `配置读取失败：${(err as Error).message}` })
    }
  }, [isBackendReady])

  useEffect(() => {
    void refresh()
  }, [refresh])

  const handleSave = async (): Promise<void> => {
    if (!settings || saving) return
    setSaving(true)
    setFeedback(null)
    try {
      // 以最新配置为基底，只覆盖本面板负责的字段。
      const fresh = await window.ximo.getSettings()
      const merged: RuntimeSettingsPayload = {
        ...fresh,
        providers,
        sub_agent: { pool, by_division: byDivision, by_expert: fresh.sub_agent?.by_expert ?? {} }
      }
      await window.ximo.applySettings(merged)
      setFeedback({ type: 'success', msg: '子代理模型分配已热重载并写入 config.json' })
      await refresh()
    } catch (err) {
      setFeedback({ type: 'error', msg: `保存失败：${(err as Error).message}` })
    } finally {
      setSaving(false)
    }
  }

  const uniqueCandidateId = (base: string): string => {
    const taken = new Set(providers.map((p) => p.id))
    if (!taken.has(base)) return base
    let n = 2
    while (taken.has(`${base}-${n}`)) n++
    return `${base}-${n}`
  }

  const handleFetchCandidates = async (): Promise<void> => {
    if (fetchingModels) return
    setFetchingModels(true)
    setModelPick(null)
    try {
      // 以主服务商（首行）当前填写的地址为准 —— 与凭据面板「获取模型」同口径，
      // 不要求先保存。地址为空时后端回落到已保存配置。
      const baseUrl = providers[0]?.base_url ?? ''
      const res = await window.ximo.listModels({ base_url: baseUrl })
      if (res.error) {
        setFeedback({ type: 'error', msg: `自动获取候选失败：${res.error}` })
        return
      }
      const models = res.models ?? []
      if (models.length === 0) {
        setFeedback({ type: 'error', msg: '服务商返回了空的模型列表，无法自动添加候选' })
        return
      }
      setModelPick(res)
      const existing = new Set(providers.map((p) => p.model))
      const preset: Record<string, boolean> = {}
      for (const m of models) preset[m.id] = !existing.has(m.id)
      setPicked(preset)
    } catch (err) {
      setFeedback({ type: 'error', msg: `自动获取候选失败：${(err as Error).message}` })
    } finally {
      setFetchingModels(false)
    }
  }

  const confirmAddCandidates = (): void => {
    if (!modelPick?.models) return
    const baseUrl = modelPick.base_url || providers[0]?.base_url || ''
    const secretRef = providers[0]?.secret_ref ?? ''
    const toAdd = modelPick.models.filter((m) => picked[m.id])
    if (toAdd.length === 0) return
    setProviders((prev) => {
      const next = [...prev]
      for (const m of toAdd) {
        next.push({
          id: uniqueCandidateId(m.id),
          name: m.id,
          base_url: baseUrl,
          model: m.id,
          secret_ref: secretRef,
          context_window: 131072,
          max_output_tokens: 8192,
          rate_limit_per_sec: 0
        })
      }
      return next
    })
    setModelPick(null)
    setPicked({})
    setFeedback({
      type: 'success',
      msg: `已添加 ${toAdd.length} 个候选（尚未保存，点「保存分配」生效）`
    })
  }

  const updateProvider = (index: number, patch: Partial<ProviderEntryPayload>): void => {
    setProviders((prev) => prev.map((p, i) => (i === index ? { ...p, ...patch } : p)))
  }

  const addProvider = (): void => {
    setProviders((prev) => [
      ...prev,
      {
        id: `provider-${prev.length + 1}`,
        name: '',
        base_url: '',
        model: '',
        secret_ref: prev[0]?.secret_ref ?? '',
        context_window: 131072,
        max_output_tokens: 8192,
        rate_limit_per_sec: 0
      }
    ])
  }

  const removeProvider = (index: number): void => {
    if (index === 0) return // 主服务商不可删除，只能编辑
    const removed = providers[index]?.id
    setProviders((prev) => prev.filter((_, i) => i !== index))
    // 从所有分配里同步摘掉被删候选，避免留下指向空候选的死配置。
    setPool((prev) => prev.filter((id) => id !== removed))
    setByDivision((prev) => {
      const next: Record<string, string[]> = {}
      for (const [k, v] of Object.entries(prev)) next[k] = v.filter((id) => id !== removed)
      return next
    })
  }

  const reorderProviders = (index: number, delta: number): void => {
    setProviders((prev) => {
      const to = index + delta
      if (to < 0 || to >= prev.length) return prev
      const next = [...prev]
      const [item] = next.splice(index, 1)
      next.splice(to, 0, item)
      return next
    })
  }

  if (!isBackendReady) {
    return (
      <div className="flex items-center gap-2 rounded-panel border border-border/[0.1] bg-canvas p-4 text-[12.5px] text-ink-muted">
        <AlertCircle size={15} className="shrink-0 text-warning" />
        <span>Go 后端引擎未就绪。启动后方可配置子代理模型池。</span>
      </div>
    )
  }

  const unassignedDivisions = DIVISIONS.filter((d) => !(d in byDivision))
  const poolIds = providers.map((p) => p.id).filter(Boolean)

  return (
    <div className="flex flex-col gap-4 text-[13px]">
      <p className="text-[12px] leading-relaxed text-ink-muted">
        复杂任务会拆给多位专家子代理并行处理（同时最多 8 个）。给子代理配置多个
        候选模型后，某个候选被限流（429）或断线时自动换下一个把任务跑完，
        你只会感觉慢了一点，而不会收到失败。
      </p>

      {/* 候选服务商池 */}
      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between">
          <span className="flex items-center gap-1.5 text-[13px] font-semibold text-ink">
            <Bot size={14} className="text-accent" />
            候选服务商池
          </span>
          <span className="flex items-center gap-1">
            <button
              type="button"
              onClick={() => void handleFetchCandidates()}
              disabled={fetchingModels}
              className="flex items-center gap-1 rounded-subtle border border-accent/40 bg-accent/10 px-2.5 py-1 text-[12px] font-medium text-accent transition-opacity hover:opacity-80 disabled:opacity-50"
              title="向主服务商拉取模型列表，勾选后批量生成为候选（无需手动复制粘贴）"
            >
              {fetchingModels ? (
                <Loader2 size={12} className="animate-spin" />
              ) : (
                <Sparkles size={12} />
              )}
              <span>{fetchingModels ? '获取中…' : '自动获取候选'}</span>
            </button>
            <button
              type="button"
              onClick={addProvider}
              className="flex items-center gap-1 text-[12px] font-medium text-accent transition-opacity hover:opacity-75"
            >
              <Plus size={12} />
              <span>添加候选</span>
            </button>
          </span>
        </div>

        {/* 自动获取候选：勾选面板（拉取成功后出现） */}
        {modelPick?.models && modelPick.models.length > 0 && (
          <div className="rounded-panel border border-accent/30 bg-canvas p-3">
            <div className="flex items-center justify-between">
              <span className="text-[12.5px] font-medium text-ink">
                已获取 {modelPick.models.length} 个模型（来自{' '}
                <span className="font-mono text-[11.5px]">{modelPick.base_url}</span>），勾选要加入
                候选池的模型
              </span>
              <span className="flex items-center gap-1.5">
                <button
                  type="button"
                  onClick={() => {
                    const all: Record<string, boolean> = {}
                    for (const m of modelPick.models ?? []) all[m.id] = true
                    setPicked(all)
                  }}
                  className="text-[12px] text-ink-muted transition-colors hover:text-ink"
                >
                  全选
                </button>
                <button
                  type="button"
                  onClick={() => setPicked({})}
                  className="text-[12px] text-ink-muted transition-colors hover:text-ink"
                >
                  清空
                </button>
              </span>
            </div>
            <div className="mt-2 grid max-h-[200px] grid-cols-1 gap-1 overflow-y-auto sm:grid-cols-2">
              {modelPick.models.map((m) => (
                <label
                  key={m.id}
                  className="flex cursor-pointer items-center gap-2 rounded-subtle px-2 py-1.5 transition-colors hover:bg-surface"
                >
                  <input
                    type="checkbox"
                    checked={picked[m.id] ?? false}
                    onChange={(e) =>
                      setPicked((prev) => ({ ...prev, [m.id]: e.target.checked }))
                    }
                    className="h-3.5 w-3.5 accent-[var(--accent,#6366f1)]"
                  />
                  <span className="truncate font-mono text-[12px] text-ink">{m.id}</span>
                  {m.owned_by && (
                    <span className="ml-auto shrink-0 text-[11px] text-ink-faint">{m.owned_by}</span>
                  )}
                </label>
              ))}
            </div>
            <div className="mt-2 flex items-center justify-between">
              <span className="text-[11.5px] text-ink-faint">
                已勾选 {Object.values(picked).filter(Boolean).length} 个；已存在的模型会自动跳过
              </span>
              <span className="flex items-center gap-1.5">
                <button
                  type="button"
                  onClick={() => setModelPick(null)}
                  className="rounded-subtle px-2.5 py-1 text-[12px] text-ink-muted transition-colors hover:text-ink"
                >
                  取消
                </button>
                <button
                  type="button"
                  onClick={confirmAddCandidates}
                  disabled={Object.values(picked).filter(Boolean).length === 0}
                  className="glass-accent-solid flex items-center gap-1 rounded-subtle px-3 py-1 text-[12px] font-medium disabled:opacity-40"
                >
                  <Plus size={11} />
                  <span>添加为候选</span>
                </button>
              </span>
            </div>
          </div>
        )}

        {providers.length === 0 && (
          <p className="rounded-subtle border border-border/[0.1] bg-canvas px-3 py-2 text-[12px] text-ink-muted">
            尚未读到候选池。请先在「模型与 API 凭据」中配置接口地址后刷新。
          </p>
        )}

        {providers.map((p, i) => (
          <div
            key={`${p.id}-${i}`}
            className="rounded-panel border border-border/[0.09] bg-canvas p-3"
          >
            <div className="flex items-center justify-between gap-2">
              <span className="flex items-center gap-2 text-[12px] font-medium text-ink">
                {i === 0 ? (
                  <span className="rounded-full border border-accent/40 bg-accent/10 px-2 py-0.5 text-[11px] text-accent">
                    主服务商
                  </span>
                ) : (
                  <span className="rounded-full border border-border/[0.12] px-2 py-0.5 text-[11px] text-ink-muted">
                    候选 {i}
                  </span>
                )}
              </span>
              <span className="flex items-center gap-1">
                <button
                  type="button"
                  onClick={() => reorderProviders(i, -1)}
                  disabled={i === 0}
                  className="p-1 text-ink-muted transition-colors hover:text-ink disabled:opacity-30"
                  title="上移（提高优先级）"
                >
                  <ArrowUp size={13} />
                </button>
                <button
                  type="button"
                  onClick={() => reorderProviders(i, 1)}
                  disabled={i === providers.length - 1}
                  className="p-1 text-ink-muted transition-colors hover:text-ink disabled:opacity-30"
                  title="下移（降低优先级）"
                >
                  <ArrowDown size={13} />
                </button>
                <button
                  type="button"
                  onClick={() => removeProvider(i)}
                  disabled={i === 0}
                  className="p-1 text-ink-muted transition-colors hover:text-danger disabled:opacity-30"
                  title={i === 0 ? '主服务商不可删除' : '删除候选'}
                >
                  <Trash2 size={13} />
                </button>
              </span>
            </div>

            <div className="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-3">
              <input
                type="text"
                value={p.id}
                onChange={(e) => updateProvider(i, { id: e.target.value })}
                placeholder="候选标识"
                disabled={i === 0}
                className="glass-inset px-2.5 py-1.5 font-mono text-[12px] text-ink outline-none placeholder:text-ink-faint disabled:opacity-60"
              />
              <input
                type="text"
                value={p.base_url}
                onChange={(e) => updateProvider(i, { base_url: e.target.value })}
                placeholder="接口地址 https://…"
                className="glass-inset px-2.5 py-1.5 font-mono text-[12px] text-ink outline-none placeholder:text-ink-faint"
              />
              <input
                type="text"
                value={p.model}
                onChange={(e) => updateProvider(i, { model: e.target.value })}
                placeholder="模型名"
                className="glass-inset px-2.5 py-1.5 font-mono text-[12px] text-ink outline-none placeholder:text-ink-faint"
              />
            </div>
          </div>
        ))}
      </div>

      <div className="h-px bg-border/[0.07]" />

      {/* 全局候选顺序 */}
      <CandidateOrderEditor
        title="全局子代理候选顺序"
        hint="未单独分配的分类按此顺序取候选；留空则按候选池顺序。"
        poolIds={poolIds}
        order={pool}
        onChange={setPool}
      />

      {/* 分类覆盖 */}
      <div className="flex flex-col gap-2">
        <div className="flex items-center justify-between">
          <span className="text-[13px] font-semibold text-ink">按专家分类覆盖</span>
          <select
            value=""
            onChange={(e) => {
              if (!e.target.value) return
              setByDivision((prev) => ({ ...prev, [e.target.value]: [...poolIds].slice(0, 1) }))
            }}
            disabled={unassignedDivisions.length === 0}
            className="glass-inset px-2 py-1 text-[12px] text-ink outline-none disabled:opacity-50"
          >
            <option value="">为分类添加覆盖…</option>
            {unassignedDivisions.map((d) => (
              <option key={d} value={d}>
                {DIVISION_LABELS[d] ?? d}
              </option>
            ))}
          </select>
        </div>

        {Object.keys(byDivision).length === 0 && (
          <p className="rounded-subtle border border-border/[0.1] bg-canvas px-3 py-2 text-[12px] text-ink-muted">
            尚无分类覆盖 —— 所有分类都使用全局候选顺序。
          </p>
        )}

        {Object.entries(byDivision).map(([division, order]) => (
          <div key={division} className="rounded-panel border border-border/[0.09] bg-canvas p-3">
            <div className="flex items-center justify-between">
              <span className="text-[12.5px] font-medium text-ink">
                {DIVISION_LABELS[division] ?? division}
              </span>
              <button
                type="button"
                onClick={() =>
                  setByDivision((prev) => {
                    const next = { ...prev }
                    delete next[division]
                    return next
                  })
                }
                className="p-1 text-ink-muted transition-colors hover:text-danger"
                title="移除该分类覆盖"
              >
                <Trash2 size={13} />
              </button>
            </div>
            <div className="mt-2">
              <CandidateChips
                poolIds={poolIds}
                order={order}
                onChange={(next) => setByDivision((prev) => ({ ...prev, [division]: next }))}
              />
            </div>
          </div>
        ))}
      </div>

      <div className="flex items-center justify-between gap-3 pt-1">
        <span className="truncate font-mono text-[12px] text-ink-faint" title={settings?.config_path}>
          {settings?.config_path}
        </span>
        <button
          type="button"
          onClick={() => void handleSave()}
          disabled={saving}
          className="glass-accent-solid flex shrink-0 items-center gap-1.5 px-4 py-2 text-[12.5px] font-medium disabled:opacity-40"
        >
          {saving ? <Loader2 size={13} className="animate-spin" /> : <Save size={13} />}
          <span>保存分配</span>
        </button>
      </div>

      {feedback && (
        <p
          className={`text-[12px] font-medium ${
            feedback.type === 'success' ? 'text-success' : 'text-danger'
          }`}
        >
          {feedback.msg}
        </p>
      )}
    </div>
  )
}

interface CandidateOrderEditorProps {
  title: string
  hint?: string
  poolIds: string[]
  order: string[]
  onChange: (next: string[]) => void
}

/** 单张「有序候选」编辑器：芯片列表 + 上移/下移/移除 + 追加。 */
function CandidateOrderEditor(props: CandidateOrderEditorProps): React.JSX.Element {
  const { title, hint, poolIds, order, onChange } = props
  return (
    <div className="rounded-panel border border-border/[0.09] bg-canvas p-3">
      <div className="flex items-center justify-between">
        <span className="text-[13px] font-semibold text-ink">{title}</span>
        <select
          value=""
          onChange={(e) => {
            if (!e.target.value || order.includes(e.target.value)) return
            onChange([...order, e.target.value])
          }}
          className="glass-inset px-2 py-1 text-[12px] text-ink outline-none"
        >
          <option value="">追加候选…</option>
          {poolIds
            .filter((id) => !order.includes(id))
            .map((id) => (
              <option key={id} value={id}>
                {id}
              </option>
            ))}
        </select>
      </div>
      {hint && <p className="mt-1 text-[12px] leading-relaxed text-ink-muted">{hint}</p>}
      <div className="mt-2">
        <CandidateChips poolIds={poolIds} order={order} onChange={onChange} />
      </div>
    </div>
  )
}

interface CandidateChipsProps {
  poolIds: string[]
  order: string[]
  onChange: (next: string[]) => void
}

function CandidateChips(props: CandidateChipsProps): React.JSX.Element {
  const { order, onChange } = props
  if (order.length === 0) {
    return <p className="text-[12px] text-ink-faint">（空 —— 使用候选池默认顺序）</p>
  }
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      {order.map((id, i) => (
        <span
          key={`${id}-${i}`}
          className="flex items-center gap-1 rounded-full border border-border/[0.12] bg-surface px-2.5 py-1 font-mono text-[11.5px] text-ink"
        >
          <span className="text-ink-faint">{i + 1}.</span>
          {id}
          <button
            type="button"
            onClick={() => onChange([...order.slice(0, i), ...order.slice(i + 1)])}
            className="text-ink-muted transition-colors hover:text-danger"
            title="移除"
          >
            <Trash2 size={11} />
          </button>
        </span>
      ))}
      {order.length > 1 && (
        <span className="ml-1 flex items-center gap-0.5">
          <button
            type="button"
            onClick={() => onChange([...order.slice(1), order[0]])}
            className="p-1 text-ink-muted transition-colors hover:text-ink"
            title="整体左移一位"
          >
            <ArrowUp size={12} />
          </button>
          <button
            type="button"
            onClick={() => onChange([...order.slice(-1), ...order.slice(0, -1)])}
            className="p-1 text-ink-muted transition-colors hover:text-ink"
            title="整体右移一位"
          >
            <ArrowDown size={12} />
          </button>
        </span>
      )}
    </div>
  )
}
