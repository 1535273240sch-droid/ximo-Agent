import { useMemo, useState } from 'react'
import { Search, Trash2 } from 'lucide-react'
import { ALL_MEMORY_KINDS, type MemoryGraphNode, type MemoryKind } from '@shared/types'
import {
  MEMORY_KIND_LABELS,
  MEMORY_STATUS_LABELS,
  filterNodes,
  kindAccentClass
} from '../../store/memory-store'
import { fmtRelative, oneLine, weightPercent } from './format'

/**
 * 列表视图（审核文档 4.9 第 3 条）：按 kind / 实体 / 主题筛选，方便批量清理。
 *
 * 与图视图共用同一个 store 的同一份 nodes：切视图不重新拉数据，筛选也只影响
 * 本视图的呈现（图视图有自己的 kind 筛选）。这样「图上看到的那批」和「列表里
 * 看到的那批」永远是同一份事实，不会出现两个视图各说各话。
 */
interface MemoryListViewProps {
  nodes: MemoryGraphNode[]
  selectedId: string | null
  listKinds: MemoryKind[]
  query: string
  /** 图数据被 Limit 截断时为 true，列表会提示还有多少未加载。 */
  truncated: boolean
  total: number
  onKindsChange: (kinds: MemoryKind[]) => void
  onQueryChange: (q: string) => void
  onSelect: (id: string) => void
  onDelete: (id: string) => void
  onIncludeArchivedChange: (v: boolean) => void
  includeArchived: boolean
}

export function MemoryListView({
  nodes,
  selectedId,
  listKinds,
  query,
  truncated,
  total,
  onKindsChange,
  onQueryChange,
  onSelect,
  onDelete,
  onIncludeArchivedChange,
  includeArchived
}: MemoryListViewProps): React.JSX.Element {
  const [confirmId, setConfirmId] = useState<string | null>(null)

  const visible = useMemo(
    () => filterNodes(nodes, { kinds: listKinds, query }),
    [nodes, listKinds, query]
  )

  const toggleKind = (k: MemoryKind): void => {
    onKindsChange(listKinds.includes(k) ? listKinds.filter((x) => x !== k) : [...listKinds, k])
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* 筛选条 */}
      <div className="shrink-0 border-b border-border/[0.08] px-4 py-2.5">
        <div className="flex flex-wrap items-center gap-2">
          <div className="glass-inset flex items-center gap-2 px-2.5 py-1">
            <Search size={13} className="shrink-0 text-ink-muted" />
            <input
              type="text"
              value={query}
              onChange={(e) => onQueryChange(e.target.value)}
              placeholder="按标题或内容过滤…"
              aria-label="过滤记忆"
              className="w-[200px] bg-transparent text-[12.5px] text-ink outline-none placeholder:text-ink-faint"
            />
          </div>

          {ALL_MEMORY_KINDS.map((k) => {
            const active = listKinds.includes(k)
            return (
              <button
                key={k}
                type="button"
                onClick={() => toggleKind(k)}
                aria-pressed={active}
                className={[
                  'rounded-subtle border px-2 py-1 text-[12px] transition-colors',
                  active
                    ? 'border-accent bg-accent/15 text-accent'
                    : 'border-border/[0.1] text-ink-muted hover:text-ink'
                ].join(' ')}
              >
                {MEMORY_KIND_LABELS[k] ?? k}
              </button>
            )
          })}

          <label className="ml-1 flex items-center gap-1.5 text-[12px] text-ink-muted">
            <input
              type="checkbox"
              checked={includeArchived}
              onChange={(e) => onIncludeArchivedChange(e.target.checked)}
              className="accent-[rgb(var(--c-accent))]"
            />
            含已归档
          </label>

          <span className="ml-auto text-[12px] text-ink-faint">
            {visible.length} / {nodes.length} 条
            {truncated && `（共 ${total} 条，仅加载了最重要的 ${nodes.length} 条）`}
          </span>
        </div>
      </div>

      {/* 列表 */}
      <div className="min-h-0 flex-1 overflow-y-auto">
        {visible.length === 0 ? (
          <p className="px-4 py-6 text-[12.5px] text-ink-faint">没有符合条件的记忆。</p>
        ) : (
          <ul className="flex flex-col">
            {visible.map((n) => {
              const active = n.id === selectedId
              return (
                <li
                  key={n.id}
                  className={[
                    'flex items-start gap-3 border-b border-border/[0.06] px-4 py-2.5 transition-colors',
                    active ? 'bg-accent/[0.07]' : 'hover:bg-surface-raised'
                  ].join(' ')}
                >
                  <button
                    type="button"
                    onClick={() => onSelect(n.id)}
                    className="flex min-w-0 flex-1 flex-col gap-1 text-left"
                  >
                    <span className="flex items-center gap-2">
                      <span
                        className={`shrink-0 text-[11px] font-medium ${kindAccentClass(n.kind)}`}
                      >
                        {MEMORY_KIND_LABELS[n.kind] ?? n.kind}
                      </span>
                      <span className="truncate text-[13px] font-medium text-ink">
                        {n.title || oneLine(n.content_preview, 40) || '(无标题)'}
                      </span>
                      {n.pinned && <span className="shrink-0 text-[11px] text-warning">置顶</span>}
                      {n.status !== 'active' && (
                        <span className="shrink-0 text-[11px] text-ink-faint">
                          {MEMORY_STATUS_LABELS[n.status] ?? n.status}
                        </span>
                      )}
                    </span>
                    <span className="truncate text-[12px] text-ink-muted">
                      {oneLine(n.content_preview, 90)}
                    </span>
                    <span className="flex items-center gap-3 text-[11px] text-ink-faint">
                      <span>用过 {n.use_count} 次</span>
                      <span>最近 {fmtRelative(n.last_used)}</span>
                      <span>{n.degree} 条关联</span>
                      <span className="flex items-center gap-1">
                        重要度
                        <span className="inline-block h-1 w-12 overflow-hidden rounded-full bg-border/[0.12]">
                          <span
                            className="block h-full rounded-full bg-accent/70"
                            style={{ width: `${weightPercent(n.importance)}%` }}
                          />
                        </span>
                        {n.importance.toFixed(2)}
                      </span>
                    </span>
                  </button>

                  {confirmId === n.id ? (
                    <span className="flex shrink-0 items-center gap-1.5">
                      <button
                        type="button"
                        onClick={() => {
                          onDelete(n.id)
                          setConfirmId(null)
                        }}
                        className="rounded-[5px] border border-danger/40 px-2 py-0.5 text-[11.5px] text-danger hover:bg-danger/10"
                      >
                        确认遗忘
                      </button>
                      <button
                        type="button"
                        onClick={() => setConfirmId(null)}
                        className="text-[11.5px] text-ink-faint hover:text-ink"
                      >
                        取消
                      </button>
                    </span>
                  ) : (
                    <button
                      type="button"
                      onClick={() => setConfirmId(n.id)}
                      title="遗忘（硬删除，不可撤销）"
                      aria-label={`遗忘 ${n.title || n.id}`}
                      className="shrink-0 rounded-[5px] p-1 text-ink-faint transition-colors hover:text-danger"
                    >
                      <Trash2 size={13} />
                    </button>
                  )}
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </div>
  )
}
