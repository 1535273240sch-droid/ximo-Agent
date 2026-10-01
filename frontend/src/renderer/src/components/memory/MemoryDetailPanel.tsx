import { useEffect, useMemo, useState } from 'react'
import {
  Archive,
  Link2,
  Pencil,
  Pin,
  PinOff,
  Save,
  Trash2,
  X
} from 'lucide-react'
import type {
  MemoryGraphNode,
  MemoryNodeDetail,
  MemoryNodeUpdate,
  MemoryRel
} from '@shared/types'
import { ALL_MEMORY_RELS } from '@shared/types'
import {
  MEMORY_KIND_LABELS,
  MEMORY_REL_LABELS,
  MEMORY_STATUS_LABELS,
  kindAccentClass
} from '../../store/memory-store'
import { effWeight, fmtRelative, fmtTime, oneLine, weightPercent } from './format'

/**
 * 节点详情面板（审核文档 4.9 第 2 条）。
 *
 * 展示：完整正文、来源 run、关联节点（带权重条）、用过 N 次 / 最近使用。
 * 操作：编辑、置顶、归档、遗忘（二次确认）、手动连线（选另一个节点 + rel 下拉）。
 *
 * 两条刻意的设计：
 *   1. 「遗忘」是硬删除，不可撤销，因此必须二次确认，且确认按钮的文案里写明
 *      「连同它的全部关联」——用户点下去之前应该知道会失去什么。
 *   2. 编辑态是「草稿」而不是受控直改：后端 update 走的是指针三态，中途保存失败
 *      会留下一个和库里不一致的输入框。用草稿 + 显式保存，语义清楚。
 */
interface MemoryDetailPanelProps {
  detail: MemoryNodeDetail | null
  /** 可选的连线目标（来自当前图数据）。 */
  candidates: MemoryGraphNode[]
  /** 正在加载详情。 */
  loading?: boolean
  onSelect: (id: string) => void
  onUpdate: (upd: MemoryNodeUpdate) => Promise<void>
  onDelete: (id: string) => Promise<void>
  onLink: (src: string, dst: string, rel: MemoryRel) => Promise<void>
}

export function MemoryDetailPanel({
  detail,
  candidates,
  loading,
  onSelect,
  onUpdate,
  onDelete,
  onLink
}: MemoryDetailPanelProps): React.JSX.Element {
  const [editing, setEditing] = useState(false)
  const [draftTitle, setDraftTitle] = useState('')
  const [draftContent, setDraftContent] = useState('')
  const [draftImportance, setDraftImportance] = useState(0.5)
  const [confirmForget, setConfirmForget] = useState(false)
  const [linking, setLinking] = useState(false)
  const [linkTarget, setLinkTarget] = useState('')
  const [linkRel, setLinkRel] = useState<MemoryRel>('related')
  const [busy, setBusy] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)

  const node = detail?.node
  // 切换节点时重置所有临时状态：否则会带着上一个节点的编辑草稿/确认态。
  useEffect(() => {
    setEditing(false)
    setConfirmForget(false)
    setLinking(false)
    setLocalError(null)
    if (detail) {
      setDraftTitle(detail.node.title ?? '')
      setDraftContent(detail.content ?? '')
      setDraftImportance(detail.node.importance)
      setLinkTarget('')
      setLinkRel('related')
    }
  }, [detail?.node.id, detail])

  /** 关联列表：边 + 对端节点，按有效权重从高到低。 */
  const relations = useMemo(() => {
    if (!detail || !node) return []
    const byId = new Map(detail.neighbors.map((n) => [n.id, n]))
    return detail.edges
      .map((e) => {
        const otherId = e.src === node.id ? e.dst : e.src
        return {
          edge: e,
          otherId,
          other: byId.get(otherId),
          outgoing: e.src === node.id
        }
      })
      .sort((a, b) => effWeight(b.edge) - effWeight(a.edge))
  }, [detail, node])

  /** 可连线的候选：排除自己与已经连上的节点。 */
  const linkCandidates = useMemo(() => {
    if (!node) return []
    const connected = new Set(relations.map((r) => r.otherId))
    return candidates.filter((c) => c.id !== node.id && !connected.has(c.id))
  }, [candidates, node, relations])

  if (!detail || !node) {
    return (
      <div className="flex h-full flex-col items-center justify-center gap-1 px-4 text-center">
        <p className="text-[13px] text-ink-muted">
          {loading ? '正在载入节点详情…' : '在左侧图中点一个节点，这里会显示它的完整内容'}
        </p>
        <p className="text-[12px] text-ink-faint">
          关联节点、使用次数与召回历史都在这一栏
        </p>
      </div>
    )
  }

  const run = (fn: () => Promise<void>): void => {
    setBusy(true)
    setLocalError(null)
    void fn()
      .catch((err: unknown) => {
        setLocalError(err instanceof Error ? err.message : String(err))
      })
      .finally(() => setBusy(false))
  }

  const handleSave = (): void => {
    run(async () => {
      await onUpdate({
        node_id: node.id,
        title: draftTitle,
        content: draftContent,
        importance: draftImportance
      })
      setEditing(false)
    })
  }

  const handlePin = (): void => {
    run(async () => {
      await onUpdate({ node_id: node.id, pinned: !node.pinned })
    })
  }

  const handleArchive = (): void => {
    run(async () => {
      await onUpdate({
        node_id: node.id,
        status: node.status === 'archived' ? 'active' : 'archived'
      })
    })
  }

  const handleForget = (): void => {
    if (!confirmForget) {
      setConfirmForget(true)
      return
    }
    run(async () => {
      await onDelete(node.id)
      setConfirmForget(false)
    })
  }

  const handleLink = (): void => {
    if (linkTarget === '') return
    run(async () => {
      await onLink(node.id, linkTarget, linkRel)
      setLinking(false)
      setLinkTarget('')
    })
  }

  const recalls = detail.recalls ?? []

  return (
    <div className="flex h-full min-h-0 flex-col">
      {/* 头部：kind 徽标 + 标题 + 状态 */}
      <div className="shrink-0 border-b border-border/[0.08] px-4 py-3">
        <div className="flex items-center gap-2">
          <span
            className={`rounded-[4px] bg-surface-raised px-1.5 py-0.5 text-[11px] font-medium ${kindAccentClass(node.kind)}`}
          >
            {MEMORY_KIND_LABELS[node.kind] ?? node.kind}
          </span>
          {node.pinned && (
            <span className="rounded-[4px] bg-warning/15 px-1.5 py-0.5 text-[11px] font-medium text-warning">
              已置顶
            </span>
          )}
          {node.status !== 'active' && (
            <span className="rounded-[4px] bg-ink-faint/15 px-1.5 py-0.5 text-[11px] font-medium text-ink-muted">
              {MEMORY_STATUS_LABELS[node.status] ?? node.status}
            </span>
          )}
          <span className="ml-auto font-mono text-[11px] text-ink-faint">{node.id}</span>
        </div>
        {!editing ? (
          <h2 className="mt-1.5 break-words text-[15px] font-semibold leading-snug text-ink">
            {node.title || '(无标题)'}
          </h2>
        ) : (
          <input
            aria-label="标题"
            value={draftTitle}
            onChange={(e) => setDraftTitle(e.target.value)}
            className="glass-inset mt-1.5 w-full px-2 py-1 text-[14px] text-ink outline-none"
            placeholder="标题"
          />
        )}
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-4 py-3">
        {/* 正文 */}
        <section className="mb-4">
          <h3 className="mb-1 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            正文
          </h3>
          {!editing ? (
            <p className="whitespace-pre-wrap break-words text-[13px] leading-relaxed text-ink">
              {detail.content || node.content_preview || '(空)'}
            </p>
          ) : (
            <textarea
              aria-label="正文"
              value={draftContent}
              onChange={(e) => setDraftContent(e.target.value)}
              rows={8}
              className="glass-inset w-full resize-y px-2 py-1.5 font-mono text-[12.5px] leading-relaxed text-ink outline-none"
            />
          )}
          {editing && (
            <label className="mt-2 flex items-center gap-2 text-[12px] text-ink-muted">
              <span className="shrink-0">重要度 {draftImportance.toFixed(2)}</span>
              <input
                aria-label="重要度"
                type="range"
                min={0}
                max={1}
                step={0.05}
                value={draftImportance}
                onChange={(e) => setDraftImportance(Number(e.target.value))}
                className="h-1 flex-1 accent-[rgb(var(--c-accent))]"
              />
            </label>
          )}
        </section>

        {/* 元信息 */}
        <section className="mb-4 grid grid-cols-2 gap-x-3 gap-y-1.5 text-[12px]">
          <Meta label="用过" value={`${node.use_count} 次`} />
          <Meta label="最近使用" value={fmtRelative(node.last_used)} />
          <Meta label="创建于" value={fmtTime(node.created_at)} />
          <Meta label="度数" value={`${node.degree} 条关联`} />
          <div className="col-span-2 flex items-center gap-1.5">
            <span className="shrink-0 text-ink-faint">来源 run</span>
            {node.source_run ? (
              <span className="truncate font-mono text-ink-muted" title={node.source_run}>
                {node.source_run}
              </span>
            ) : (
              <span className="text-ink-faint">（手动创建或旧数据迁移）</span>
            )}
          </div>
        </section>

        {/* 操作 */}
        <section className="mb-4 flex flex-wrap items-center gap-1.5">
          {!editing ? (
            <ActionButton icon={Pencil} label="编辑" onClick={() => setEditing(true)} disabled={busy} />
          ) : (
            <>
              <ActionButton icon={Save} label="保存" onClick={handleSave} disabled={busy} primary />
              <ActionButton icon={X} label="取消" onClick={() => setEditing(false)} disabled={busy} />
            </>
          )}
          <ActionButton
            icon={node.pinned ? PinOff : Pin}
            label={node.pinned ? '取消置顶' : '置顶'}
            onClick={handlePin}
            disabled={busy}
          />
          <ActionButton
            icon={Archive}
            label={node.status === 'archived' ? '取消归档' : '归档'}
            onClick={handleArchive}
            disabled={busy}
          />
          <ActionButton
            icon={Link2}
            label="手动连线"
            onClick={() => setLinking((v) => !v)}
            disabled={busy || linkCandidates.length === 0}
            title={linkCandidates.length === 0 ? '当前图里没有可连线的其它节点' : undefined}
          />
          <ActionButton
            icon={Trash2}
            label={confirmForget ? '确认遗忘' : '遗忘'}
            onClick={handleForget}
            danger
            disabled={busy}
            title="硬删除该节点及其全部关联，不可撤销"
          />
          {confirmForget && (
            <button
              type="button"
              onClick={() => setConfirmForget(false)}
              className="text-[11.5px] text-ink-faint underline-offset-2 hover:underline"
            >
              取消
            </button>
          )}
        </section>

        {confirmForget && (
          <p className="mb-4 rounded-[6px] border border-danger/30 bg-danger/10 px-2.5 py-2 text-[12px] leading-relaxed text-danger">
            这会永久删除「{node.title || node.id}」以及它的 {node.degree} 条关联，无法撤销。
            再点一次「确认遗忘」才会执行。
          </p>
        )}

        {/* 手动连线表单 */}
        {linking && (
          <section className="mb-4 flex flex-col gap-2 rounded-[6px] border border-border/[0.1] bg-surface/60 p-2.5">
            <div className="flex items-center gap-2">
              <label className="shrink-0 text-[12px] text-ink-muted" htmlFor="memory-link-target">
                连接到
              </label>
              <select
                id="memory-link-target"
                aria-label="连接到"
                value={linkTarget}
                onChange={(e) => setLinkTarget(e.target.value)}
                className="glass-inset min-w-0 flex-1 px-2 py-1 text-[12.5px] text-ink outline-none"
              >
                <option value="">选择节点…</option>
                {linkCandidates.map((c) => (
                  <option key={c.id} value={c.id}>
                    {MEMORY_KIND_LABELS[c.kind] ?? c.kind} · {c.title || oneLine(c.content_preview, 40) || c.id}
                  </option>
                ))}
              </select>
            </div>
            <div className="flex items-center gap-2">
              <label className="shrink-0 text-[12px] text-ink-muted" htmlFor="memory-link-rel">
                关系
              </label>
              <select
                id="memory-link-rel"
                aria-label="关系"
                value={linkRel}
                onChange={(e) => setLinkRel(e.target.value as MemoryRel)}
                className="glass-inset flex-1 px-2 py-1 text-[12.5px] text-ink outline-none"
              >
                {ALL_MEMORY_RELS.map((r) => (
                  <option key={r} value={r}>
                    {MEMORY_REL_LABELS[r] ?? r}
                  </option>
                ))}
              </select>
              <button
                type="button"
                onClick={handleLink}
                disabled={busy || linkTarget === ''}
                className="glass-accent-solid px-2.5 py-1 text-[12px] font-medium disabled:opacity-40"
              >
                建立
              </button>
            </div>
          </section>
        )}

        {localError && (
          <p className="mb-4 rounded-[6px] border border-danger/30 bg-danger/10 px-2.5 py-2 text-[12px] text-danger">
            {localError}
          </p>
        )}

        {/* 关联节点（带权重条） */}
        <section className="mb-4">
          <h3 className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            关联节点 {relations.length > 0 && `(${relations.length})`}
          </h3>
          {relations.length === 0 ? (
            <p className="text-[12.5px] text-ink-faint">还没有关联。可以用「手动连线」建立一条。</p>
          ) : (
            <ul className="flex flex-col gap-1.5">
              {relations.map((r) => (
                <li key={`${r.edge.src}-${r.edge.dst}-${r.edge.rel}`}>
                  <button
                    type="button"
                    onClick={() => onSelect(r.otherId)}
                    className="group flex w-full flex-col gap-1 rounded-[6px] px-1.5 py-1 text-left transition-colors hover:bg-surface-raised"
                  >
                    <span className="flex items-center gap-1.5">
                      <span className="shrink-0 text-[11px] text-ink-faint">
                        {r.outgoing ? '→' : '←'} {MEMORY_REL_LABELS[r.edge.rel] ?? r.edge.rel}
                      </span>
                      <span className="truncate text-[12.5px] text-ink">
                        {r.other?.title || oneLine(r.other?.content_preview, 30) || r.otherId}
                      </span>
                      <span className="ml-auto shrink-0 font-mono text-[11px] text-ink-faint">
                        {effWeight(r.edge).toFixed(2)}
                      </span>
                    </span>
                    {/* 权重条用有效权重：一条早已衰减的边不该看起来还很强。 */}
                    <span className="h-1 w-full overflow-hidden rounded-full bg-border/[0.12]">
                      <span
                        className="block h-full rounded-full bg-accent/70"
                        style={{ width: `${weightPercent(effWeight(r.edge))}%` }}
                      />
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </section>

        {/* 召回历史 */}
        <section className="pb-4">
          <h3 className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-ink-faint">
            召回历史 {recalls.length > 0 && `(${recalls.length})`}
          </h3>
          {recalls.length === 0 ? (
            <p className="text-[12.5px] text-ink-faint">还没有被想起过。</p>
          ) : (
            <ul className="flex flex-col gap-1">
              {recalls.map((r, i) => (
                <li key={`${r.run_id}-${r.ts}-${i}`} className="text-[11.5px] leading-relaxed text-ink-muted">
                  <span className="text-ink-faint">{fmtTime(r.ts)}</span>
                  {r.used && <span className="ml-1.5 text-success">已采用</span>}
                  {r.via && <span className="ml-1.5 text-ink-faint">↳ {r.via}</span>}
                  {r.run_id && <span className="ml-1.5 font-mono text-ink-faint">{r.run_id}</span>}
                </li>
              ))}
            </ul>
          )}
        </section>
      </div>
    </div>
  )
}

function Meta({ label, value }: { label: string; value: string }): React.JSX.Element {
  return (
    <div className="flex items-center gap-1.5">
      <span className="text-ink-faint">{label}</span>
      <span className="text-ink-muted">{value}</span>
    </div>
  )
}

function ActionButton({
  icon: Icon,
  label,
  onClick,
  disabled,
  danger,
  primary,
  title
}: {
  icon: typeof Pencil
  label: string
  onClick: () => void
  disabled?: boolean
  danger?: boolean
  primary?: boolean
  title?: string
}): React.JSX.Element {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      title={title}
      className={[
        'flex items-center gap-1 rounded-[6px] border px-2 py-1 text-[12px] transition-colors disabled:opacity-40',
        danger
          ? 'border-danger/40 text-danger hover:bg-danger/10'
          : primary
            ? 'border-accent/50 bg-accent/15 text-accent hover:bg-accent/25'
            : 'border-border/[0.12] text-ink-muted hover:bg-surface-raised hover:text-ink'
      ].join(' ')}
    >
      <Icon size={12} />
      <span>{label}</span>
    </button>
  )
}
