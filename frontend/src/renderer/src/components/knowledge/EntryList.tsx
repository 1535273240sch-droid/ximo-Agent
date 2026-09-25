import { SearchX } from 'lucide-react'
import type { KnowledgeEntry } from './KnowledgeView'

function relativeTime(ts: number): string {
  const diffSec = Math.floor((Date.now() - ts) / 1000)
  if (diffSec < 60) return '刚刚'
  if (diffSec < 3600) return `${Math.floor(diffSec / 60)} 分钟前`
  if (diffSec < 86400) return `${Math.floor(diffSec / 3600)} 小时前`
  const days = Math.floor(diffSec / 86400)
  if (days < 30) return `${days} 天前`
  return `${Math.floor(days / 30)} 个月前`
}

interface EntryListProps {
  entries: KnowledgeEntry[]
  selectedId: string | null
  onSelect: (id: string) => void
}

export function EntryList({ entries, selectedId, onSelect }: EntryListProps): React.JSX.Element {
  if (entries.length === 0) {
    return (
      <div className="flex h-full flex-col items-center justify-center p-6 text-center select-none">
        <SearchX size={26} className="text-ink-faint mb-2" />
        <p className="text-[13px] font-medium text-ink">未检索到匹配的条目</p>
        <p className="mt-1 text-[12px] leading-relaxed text-ink-muted">
          尝试清空搜索词，或点击上方「新建条目」录入第一条规范。
        </p>
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col overflow-y-auto divide-y divide-border/[0.08]">
      {entries.map((entry) => {
        const isSelected = entry.id === selectedId
        return (
          <button
            key={entry.id}
            type="button"
            onClick={() => onSelect(entry.id)}
            className={`flex flex-col gap-1.5 p-4 text-left transition-colors ${
              isSelected ? 'bg-surface-raised border-l-2 border-l-accent' : 'hover:bg-surface-raised/60'
            }`}
          >
            <div className="flex items-start justify-between gap-2">
              <span className={`line-clamp-1 font-display text-[13.5px] ${isSelected ? 'font-semibold text-ink' : 'font-medium text-ink'}`}>
                {entry.title}
              </span>
              <span className="font-mono text-[12px] shrink-0 text-ink-faint">
                {relativeTime(entry.updatedAt)}
              </span>
            </div>

            <p className="line-clamp-2 text-[12px] leading-relaxed text-ink-muted">
              {entry.content}
            </p>

            {entry.tags.length > 0 && (
              <div className="flex flex-wrap gap-1 pt-1">
                {entry.tags.map((t) => (
                  <span
                    key={t}
                    className="rounded border border-border/[0.1] bg-canvas px-1.5 py-0.5 font-mono text-[12px] text-ink-muted"
                  >
                    #{t}
                  </span>
                ))}
              </div>
            )}
          </button>
        )
      })}
    </div>
  )
}
