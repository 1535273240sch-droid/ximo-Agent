import { useCallback, useEffect, useRef, useState } from 'react'
import {
  Crosshair,
  List,
  Loader2,
  Moon,
  Network,
  RefreshCw,
  Settings as SettingsIcon,
  X
} from 'lucide-react'
import { ALL_MEMORY_KINDS, type DurableEvent, type MemoryKind } from '@shared/types'
import {
  MEMORY_KIND_LABELS,
  bridgeFromWindow,
  setMemoryBridge,
  useMemoryStore
} from '../../store/memory-store'
import { CanvasGraph } from './CanvasGraph'
import { MemoryDetailPanel } from './MemoryDetailPanel'
import { MemoryListView } from './MemoryListView'
import { MemorySettingsPanel } from './MemorySettingsPanel'
import { oneLine } from './format'

/** 记忆页的三个分栏。 */
type MemoryTab = 'graph' | 'list' | 'settings'

/**
 * 记忆页（审核文档 4.9）。
 *
 * 结构：顶栏（标题 / 搜索 / 分栏切换）→ 主区（图 or 列表 or 设置）+ 右侧详情。
 *
 * 三个跨组件的行为都在这里收口，因为它们都要「在正确的时机做一次」：
 *   1. 桥注入：把 window.ximo 交给 store，之后所有动作都走 store（组件不再直接
 *      调桥），这样单测注入假桥就能覆盖全链路。
 *   2. Work Log 联动：消费 store 里的 focusNodeId，以该节点为中心取邻域并选中。
 *   3. memory.recalled 点亮：订阅事件，把被召回的节点交给 store 做脉冲排期。
 */
export function MemoryView(): React.JSX.Element {
  const [tab, setTab] = useState<MemoryTab>('graph')

  const nodes = useMemoryStore((s) => s.nodes)
  const edges = useMemoryStore((s) => s.edges)
  const total = useMemoryStore((s) => s.total)
  const truncated = useMemoryStore((s) => s.truncated)
  const stats = useMemoryStore((s) => s.stats)
  const detail = useMemoryStore((s) => s.detail)
  const selectedId = useMemoryStore((s) => s.selectedId)
  const query = useMemoryStore((s) => s.query)
  const kinds = useMemoryStore((s) => s.kinds)
  const listKinds = useMemoryStore((s) => s.listKinds)
  const includeArchived = useMemoryStore((s) => s.includeArchived)
  const searchHits = useMemoryStore((s) => s.searchHits)
  const litNodes = useMemoryStore((s) => s.litNodes)
  const litEdges = useMemoryStore((s) => s.litEdges)
  const pulses = useMemoryStore((s) => s.pulses)
  const loading = useMemoryStore((s) => s.loading)
  const error = useMemoryStore((s) => s.error)
  const notice = useMemoryStore((s) => s.notice)
  const focusNodeId = useMemoryStore((s) => s.focusNodeId)
  const focusSeq = useMemoryStore((s) => s.focusSeq)
  const reducedMotion = useMemoryStore((s) => s.reducedMotion)

  const loadGraph = useMemoryStore((s) => s.loadGraph)
  const loadStats = useMemoryStore((s) => s.loadStats)
  const selectNode = useMemoryStore((s) => s.selectNode)
  const updateNode = useMemoryStore((s) => s.updateNode)
  const deleteNode = useMemoryStore((s) => s.deleteNode)
  const linkNodes = useMemoryStore((s) => s.linkNodes)
  const consolidate = useMemoryStore((s) => s.consolidate)
  const clearAll = useMemoryStore((s) => s.clearAll)
  const exportAll = useMemoryStore((s) => s.exportAll)
  const importDoc = useMemoryStore((s) => s.importDoc)
  const setKinds = useMemoryStore((s) => s.setKinds)
  const setListKinds = useMemoryStore((s) => s.setListKinds)
  const setQuery = useMemoryStore((s) => s.setQuery)
  const setIncludeArchived = useMemoryStore((s) => s.setIncludeArchived)
  const setReducedMotion = useMemoryStore((s) => s.setReducedMotion)
  const setMode = useMemoryStore((s) => s.setMode)
  const focusNode = useMemoryStore((s) => s.focusNode)
  const triggerRecall = useMemoryStore((s) => s.triggerRecall)
  const stopRecallAnimation = useMemoryStore((s) => s.stopRecallAnimation)
  const clearNotice = useMemoryStore((s) => s.clearNotice)

  // 桥注入：挂载时把 window.ximo 交给 store；卸载时清空，避免测试之间互相污染。
  useEffect(() => {
    setMemoryBridge(bridgeFromWindow())
    return () => {
      setMemoryBridge(undefined)
      stopRecallAnimation()
    }
  }, [stopRecallAnimation])

  /**
   * 装载图数据。
   *
   * 触发时机被刻意收窄成「分栏切换」与「筛选变化」，而不是「任意依赖变化」：
   * 这个 effect 依赖 kinds/listKinds/includeArchived，而 setMode 又会写 store，
   * 如果无条件 loadGraph，一次筛选点击会打出两次 IPC（一次用旧筛选、一次用新筛选），
   * 大库上白等一个来回。用 ref 记录上一次的签名，只有签名真的变了才重取。
   */
  const loadKeyRef = useRef('')
  useEffect(() => {
    setMode(tab === 'list' ? 'list' : 'graph')
    const key = `${tab}|${kinds.join(',')}|${listKinds.join(',')}|${includeArchived ? 1 : 0}`
    if (loadKeyRef.current === key) return
    loadKeyRef.current = key
    void loadGraph()
  }, [tab, setMode, loadGraph, kinds, listKinds, includeArchived])

  useEffect(() => {
    void loadStats()
    const timer = setInterval(() => void loadStats(), 30_000)
    return () => clearInterval(timer)
  }, [loadStats])

  // prefers-reduced-motion：关掉脉冲与流光（与 global.css 的 wl-* 同一约定）。
  useEffect(() => {
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    const mq = window.matchMedia('(prefers-reduced-motion: reduce)')
    const apply = (): void => setReducedMotion(mq.matches)
    apply()
    if (typeof mq.addEventListener === 'function') {
      mq.addEventListener('change', apply)
      return () => mq.removeEventListener('change', apply)
    }
    // 旧 WebView 只支持 addListener；两个方向都要判存在，否则卸载时会抛。
    if (typeof mq.addListener === 'function') {
      mq.addListener(apply)
      return () => mq.removeListener?.(apply)
    }
    return
  }, [setReducedMotion])

  /**
   * Work Log 联动：focusNodeId 变化时以该节点为中心取邻域并选中。
   *
   * 依赖里带 focusSeq 而不是只带 focusNodeId：用户在 Work Log 里两次点同一条记忆
   * 时 id 没变，但每次点击都应该重新聚焦一次。
   */
  useEffect(() => {
    if (!focusNodeId) return
    setTab('graph')
    setMode('graph')
    void (async () => {
      await loadGraph({ centerOn: focusNodeId, depth: 2 })
      await selectNode(focusNodeId)
    })()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusNodeId, focusSeq])

  // memory.recalled：把被召回的节点交给 store 做脉冲排期。
  useEffect(() => {
    const w = typeof window === 'undefined' ? undefined : (window as unknown as { ximo?: { onEvent?: (h: (ev: DurableEvent) => void) => () => void } }).ximo
    if (!w?.onEvent) return
    const off = w.onEvent((ev) => {
      if (ev.type !== 'memory.recalled') return
      const items = Array.isArray(ev.data?.items)
        ? (ev.data?.items as { id?: unknown; text?: unknown; via?: unknown }[])
        : []
      triggerRecall(
        items
          .filter((i) => typeof i.id === 'string' && i.id !== '')
          .map((i) => ({
            id: String(i.id),
            text: typeof i.text === 'string' ? i.text : undefined,
            via: typeof i.via === 'string' ? i.via : undefined
          }))
      )
    })
    return off
  }, [triggerRecall])

  // 搜索词变化后重新取数（后端在 query 非空时只返回命中项及其直接关联）。
  const searchTimer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const handleQueryChange = useCallback(
    (q: string) => {
      setQuery(q)
      if (searchTimer.current) clearTimeout(searchTimer.current)
      searchTimer.current = setTimeout(() => {
        void useMemoryStore.getState().loadGraph()
      }, 300)
    },
    [setQuery]
  )

  useEffect(() => {
    return () => {
      if (searchTimer.current) clearTimeout(searchTimer.current)
    }
  }, [])

  const toggleKind = (k: MemoryKind): void => {
    setKinds(kinds.includes(k) ? kinds.filter((x) => x !== k) : [...kinds, k])
  }

  const focusedNode = focusNodeId ? nodes.find((n) => n.id === focusNodeId) : undefined

  return (
    <div className="flex h-full flex-col overflow-hidden bg-canvas">
      {/* 顶栏 */}
      <div className="shrink-0 border-b border-border/[0.08] bg-surface px-5 py-3">
        <div className="flex flex-wrap items-center gap-3">
          <div className="min-w-0">
            <h1 className="font-display text-[18px] font-semibold tracking-tight text-ink">
              记忆网络
            </h1>
            <p className="mt-0.5 text-[12px] text-ink-muted">
              {stats
                ? `${stats.nodes} 个节点 · ${stats.edges} 条连接 · 后端 ${stats.backend || '—'}`
                : '正在读取记忆状态…'}
              {truncated && ` · 当前仅加载最重要的 ${nodes.length} / ${total} 个节点`}
            </p>
          </div>

          {/* 分栏切换 */}
          <div className="flex items-center gap-1 rounded-[8px] border border-border/[0.1] p-0.5">
            {(
              [
                { id: 'graph', label: '网络图', icon: Network },
                { id: 'list', label: '列表', icon: List },
                { id: 'settings', label: '设置', icon: SettingsIcon }
              ] as const
            ).map((t) => {
              const active = tab === t.id
              const Icon = t.icon
              return (
                <button
                  key={t.id}
                  type="button"
                  onClick={() => setTab(t.id)}
                  aria-pressed={active}
                  className={[
                    'flex items-center gap-1.5 rounded-[6px] px-2.5 py-1 text-[12.5px] transition-colors',
                    active ? 'bg-accent/15 font-medium text-accent' : 'text-ink-muted hover:text-ink'
                  ].join(' ')}
                >
                  <Icon size={13} />
                  {t.label}
                </button>
              )
            })}
          </div>

          <div className="ml-auto flex items-center gap-2">
            {tab === 'graph' && (
              <div className="glass-inset flex items-center gap-2 px-2.5 py-1">
                <input
                  type="text"
                  value={query}
                  onChange={(e) => handleQueryChange(e.target.value)}
                  placeholder="搜索记忆…"
                  aria-label="搜索记忆"
                  className="w-[160px] bg-transparent text-[12.5px] text-ink outline-none placeholder:text-ink-faint"
                />
                {query !== '' && (
                  <button
                    type="button"
                    onClick={() => handleQueryChange('')}
                    aria-label="清空搜索"
                    className="text-ink-faint hover:text-ink"
                  >
                    <X size={12} />
                  </button>
                )}
              </div>
            )}
            <button
              type="button"
              onClick={() => {
                void loadGraph()
                void loadStats()
              }}
              disabled={loading}
              title="重新加载"
              className="rounded-[6px] border border-border/[0.12] p-1.5 text-ink-muted transition-colors hover:text-ink disabled:opacity-40"
            >
              {loading ? <Loader2 size={14} className="animate-spin" /> : <RefreshCw size={14} />}
            </button>
          </div>
        </div>

        {/* 图视图的 kind 筛选 */}
        {tab === 'graph' && (
          <div className="mt-2.5 flex flex-wrap items-center gap-1.5">
            {ALL_MEMORY_KINDS.map((k) => {
              const active = kinds.includes(k)
              return (
                <button
                  key={k}
                  type="button"
                  onClick={() => toggleKind(k)}
                  aria-pressed={active}
                  className={[
                    'rounded-subtle border px-2 py-0.5 text-[11.5px] transition-colors',
                    active
                      ? 'border-accent bg-accent/15 text-accent'
                      : 'border-border/[0.1] text-ink-muted hover:text-ink'
                  ].join(' ')}
                >
                  {MEMORY_KIND_LABELS[k] ?? k}
                </button>
              )
            })}
            <label className="ml-1 flex items-center gap-1.5 text-[11.5px] text-ink-muted">
              <input
                type="checkbox"
                checked={includeArchived}
                onChange={(e) => setIncludeArchived(e.target.checked)}
                className="accent-[rgb(var(--c-accent))]"
              />
              含已归档
            </label>

            {focusedNode && (
              <span className="ml-1 flex items-center gap-1.5 rounded-[5px] border border-accent/40 bg-accent/10 px-2 py-0.5 text-[11.5px] text-accent">
                <Crosshair size={11} />
                已聚焦：{focusedNode.title || oneLine(focusedNode.content_preview, 24) || focusedNode.id}
                <button
                  type="button"
                  onClick={() => focusNode(null)}
                  aria-label="取消聚焦"
                  className="hover:text-ink"
                >
                  <X size={11} />
                </button>
              </span>
            )}

            {stats?.enabled === false && (
              <span className="ml-1 flex items-center gap-1 rounded-[5px] border border-warning/40 bg-warning/10 px-2 py-0.5 text-[11.5px] text-warning">
                <Moon size={11} />
                记忆已关闭
              </span>
            )}
          </div>
        )}

        {(error || notice) && (
          <div className="mt-2 flex items-center gap-2">
            {error && (
              <p className="rounded-[6px] border border-danger/30 bg-danger/10 px-2 py-1 text-[11.5px] text-danger">
                {error}
              </p>
            )}
            {notice && (
              <button
                type="button"
                onClick={clearNotice}
                className="rounded-[6px] border border-success/30 bg-success/10 px-2 py-1 text-[11.5px] text-success"
                title="点击关闭"
              >
                {notice}
              </button>
            )}
          </div>
        )}
      </div>

      {/* 主区 */}
      <div className="flex min-h-0 flex-1 overflow-hidden">
        <div className="min-w-0 flex-1 overflow-hidden">
          {tab === 'graph' && (
            <CanvasGraph
              nodes={nodes}
              edges={edges}
              selectedId={selectedId}
              searchHits={searchHits}
              litNodes={litNodes}
              litEdges={litEdges}
              pulses={pulses}
              reducedMotion={reducedMotion}
              onSelect={(id) => void selectNode(id)}
            />
          )}
          {tab === 'list' && (
            <MemoryListView
              nodes={nodes}
              selectedId={selectedId}
              listKinds={listKinds}
              query={query}
              truncated={truncated}
              total={total}
              includeArchived={includeArchived}
              onKindsChange={setListKinds}
              onQueryChange={handleQueryChange}
              onSelect={(id) => void selectNode(id)}
              onDelete={(id) => void deleteNode(id).catch(() => undefined)}
              onIncludeArchivedChange={setIncludeArchived}
            />
          )}
          {tab === 'settings' && (
            <MemorySettingsPanel
              stats={stats}
              onConsolidate={consolidate}
              onExport={exportAll}
              onImport={importDoc}
              onClear={clearAll}
            />
          )}
        </div>

        {/* 右侧详情：设置分栏下不显示（那一栏是配置，不是节点）。 */}
        {tab !== 'settings' && (
          <div className="w-[340px] shrink-0 overflow-hidden border-l border-border/[0.08] bg-surface">
            <MemoryDetailPanel
              detail={detail}
              candidates={nodes}
              loading={loading && detail === null}
              onSelect={(id) => void selectNode(id)}
              onUpdate={updateNode}
              onDelete={deleteNode}
              onLink={(src, dst, rel) => linkNodes({ src, dst, rel })}
            />
          </div>
        )}
      </div>
    </div>
  )
}
