import { DIVISION_LABELS, tintColor, type Expert } from './experts-data'

export function ExpertEmoji({
  emoji,
  color,
  size = 'md'
}: {
  emoji: string
  color: string
  size?: 'sm' | 'md' | 'lg'
}): React.JSX.Element {
  const bg = tintColor(color)
  const sizeClasses = {
    sm: 'h-8 w-8 text-[15px]',
    md: 'h-10 w-10 text-[18px]',
    lg: 'h-12 w-12 text-[22px]'
  }[size]

  return (
    <span
      className={`inline-flex shrink-0 items-center justify-center rounded-[8px] border border-border/[0.1] select-none ${sizeClasses}`}
      style={{ backgroundColor: bg }}
      aria-hidden="true"
    >
      {emoji}
    </span>
  )
}

interface ExpertCardProps {
  expert: Expert
  onClick: () => void
}

export function ExpertCard({ expert, onClick }: ExpertCardProps): React.JSX.Element {
  const divisionLabel = DIVISION_LABELS[expert.division] ?? expert.division

  const handleKeyDown = (e: React.KeyboardEvent): void => {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onClick()
    }
  }

  return (
    <div
      role="button"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={handleKeyDown}
      className="glass-panel glass-panel-hover flex flex-col justify-between gap-3 p-4 text-left transition-all cursor-pointer select-none rounded-[10px]"
    >
      <div className="flex items-start gap-3">
        <ExpertEmoji emoji={expert.emoji} color={expert.color} size="md" />
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-1.5">
            <h3 className="truncate font-display text-[14px] font-semibold text-ink">
              {expert.name}
            </h3>
          </div>
          <p className="mt-0.5 truncate text-[12px] font-medium uppercase tracking-wider text-ink-muted">
            {divisionLabel}
          </p>
        </div>
      </div>

      <p className="line-clamp-2 text-[12.5px] leading-relaxed text-ink-muted">
        {expert.description}
      </p>

      {expert.tools && expert.tools.length > 0 && (
        <div className="flex flex-wrap gap-1 pt-1">
          {expert.tools.slice(0, 3).map((tool) => (
            <span
              key={tool}
              className="rounded bg-surface-raised px-1.5 py-0.5 font-mono text-[12px] text-ink-muted border border-border/[0.08]"
            >
              {tool}
            </span>
          ))}
          {expert.tools.length > 3 && (
            <span className="self-center font-mono text-[12px] text-ink-faint">
              +{expert.tools.length - 3}
            </span>
          )}
        </div>
      )}
    </div>
  )
}
