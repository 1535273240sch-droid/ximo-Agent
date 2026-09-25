import { DIVISION_COUNTS, DIVISION_LABELS, DIVISIONS } from './experts-data'

export const ALL_DIVISIONS = ''

interface DivisionFilterProps {
  selected: string
  onSelect: (division: string) => void
}

export function DivisionFilter({ selected, onSelect }: DivisionFilterProps): React.JSX.Element {
  const totalCount = Object.values(DIVISION_COUNTS).reduce((sum, n) => sum + n, 0)

  return (
    <div className="flex items-center gap-1.5 overflow-x-auto pb-1 text-[12.5px] select-none">
      <button
        type="button"
        onClick={() => onSelect(ALL_DIVISIONS)}
        className={`flex shrink-0 items-center gap-1.5 rounded-subtle border px-3 py-1 font-medium transition-colors ${
          selected === ALL_DIVISIONS
            ? 'border-accent bg-accent/15 text-accent'
            : 'border-border/[0.08] bg-canvas text-ink-muted hover:text-ink'
        }`}
      >
        <span>全部部门</span>
        <span className="font-mono text-[12px] opacity-80">({totalCount})</span>
      </button>

      {DIVISIONS.map((div) => {
        const isActive = selected === div
        const label = DIVISION_LABELS[div] ?? div
        const count = DIVISION_COUNTS[div] ?? 0

        return (
          <button
            key={div}
            type="button"
            onClick={() => onSelect(div)}
            className={`flex shrink-0 items-center gap-1.5 rounded-subtle border px-2.5 py-1 font-medium transition-colors ${
              isActive
                ? 'border-accent bg-accent/15 text-accent'
                : 'border-border/[0.08] bg-canvas text-ink-muted hover:text-ink'
            }`}
          >
            <span>{label}</span>
            <span className="font-mono text-[12px] opacity-80">({count})</span>
          </button>
        )
      })}
    </div>
  )
}
