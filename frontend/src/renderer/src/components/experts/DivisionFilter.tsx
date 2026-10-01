// 筛选条完全由后端目录驱动：divisions 与 counts 都是 props。
//
// 为什么不再从 experts-data 取部门与计数：v2.5 的筛选条读的是渲染层内联样本的
// 统计，于是「工程研发 (58)」点开只有 6 张卡。数字与部门都必须来自当前装载的
// 那份目录，才能保证「筛选条上的数 = 点进去看到的卡片数」。
import { divisionLabel } from './experts-data'

export const ALL_DIVISIONS = ''

interface DivisionFilterProps {
  /** 目录里出现的部门（后端给的原序，第一个是目录里专家最多的部门）。 */
  divisions: string[]
  /** 每个部门的专家数（由当前目录派生）。 */
  counts: Record<string, number>
  /** 目录里的专家总数，用于「全部部门」的计数。 */
  total: number
  selected: string
  onSelect: (division: string) => void
}

export function DivisionFilter({
  divisions,
  counts,
  total,
  selected,
  onSelect
}: DivisionFilterProps): React.JSX.Element {
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
        <span className="font-mono text-[12px] opacity-80">({total})</span>
      </button>

      {divisions.map((div) => {
        const isActive = selected === div
        const label = divisionLabel(div)
        const count = counts[div] ?? 0

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
