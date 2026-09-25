import { useMemo, useState } from 'react'
import { Briefcase, Code2, Info, PenTool, Plus, Search } from 'lucide-react'
import { EntryList } from './EntryList'
import { EntryEditor } from './EntryEditor'

export type KnowledgeMode = 'office' | 'coding' | 'design'

export interface KnowledgeEntry {
  id: string
  title: string
  content: string
  tags: string[]
  mode: KnowledgeMode
  source: string
  updatedAt: number
}

const SEED_ENTRIES: KnowledgeEntry[] = [
  {
    id: 'k-off-1',
    title: '团队周报规范与核心产出模板',
    content:
      '周报必须包含三部分：本周已完成工作（附量化产出）、下周确定推进计划、需协调阻断事项。避免只记流水账，聚焦关键业务交付。',
    tags: ['周报', '团队协作', '模板'],
    mode: 'office',
    source: 'user',
    updatedAt: Date.now() - 1000 * 60 * 12
  },
  {
    id: 'k-off-2',
    title: '财务报销审批流与差旅标准',
    content:
      '机票需提前 3 天通过集中平台预订。一线城市住宿标准每晚不超过 500 元。发票必须开具完整纳税人识别号与开户行信息。',
    tags: ['财务', '报销', '差旅'],
    mode: 'office',
    source: 'user',
    updatedAt: Date.now() - 1000 * 60 * 60 * 5
  },
  {
    id: 'k-cod-1',
    title: '多进程 IPC 帧协议设计规范',
    content:
      '定长头 29 字节 BigEndian 编码：包含 Magic(0x58, 0x4D)、Version、TypeLen、ReqIDLen、SessIDLen、Seq、DeadlineMs、PayloadLen。跨进程通信必须严格校验定长头魔数。',
    tags: ['IPC', 'Go', '协议'],
    mode: 'coding',
    source: 'imported',
    updatedAt: Date.now() - 1000 * 60 * 30
  },
  {
    id: 'k-cod-2',
    title: 'SQLite WAL 模式并发事务控制准则',
    content:
      '必须开启 busy_timeout(5000) 避免立即报 SQLITE_BUSY。读写分离：单写者（MaxOpenConns=1）入列序列化写入，多读者连接池并发读取。',
    tags: ['SQLite', 'WAL', '并发'],
    mode: 'coding',
    source: 'imported',
    updatedAt: Date.now() - 1000 * 60 * 60 * 24
  },
  {
    id: 'k-des-1',
    title: '黑曜石与原纸双模式设计语汇',
    content:
      '界面杜绝过度模糊雾化。以精准 1px 细微边界、8-10px 内敛圆角与极低强度漫射投影表达结构层次。文字对比度需达到严苛标准。',
    tags: ['Design', 'Tokens', 'UI'],
    mode: 'design',
    source: 'user',
    updatedAt: Date.now() - 1000 * 60 * 60 * 48
  },
  {
    id: 'k-des-2',
    title: '字体排版层级与标点规整指南',
    content:
      '西文优先 Inter/Geist，汉字优先 Noto Sans CJK/苹方。正文严格不低于 12px，常用正文统一使用 13-14px，标题控制在 20-28px。',
    tags: ['Typography', 'CJK', '排版'],
    mode: 'design',
    source: 'user',
    updatedAt: Date.now() - 1000 * 60 * 60 * 72
  }
]

const MODES: { id: KnowledgeMode; label: string; icon: typeof Briefcase }[] = [
  { id: 'office', label: '办公语料', icon: Briefcase },
  { id: 'coding', label: '编程知识', icon: Code2 },
  { id: 'design', label: '设计规范', icon: PenTool }
]

export function KnowledgeView(): React.JSX.Element {
  const [entries, setEntries] = useState<KnowledgeEntry[]>(SEED_ENTRIES)
  const [selectedId, setSelectedId] = useState<string | null>(SEED_ENTRIES[0]?.id ?? null)
  const [isCreating, setIsCreating] = useState(false)
  const [mode, setMode] = useState<KnowledgeMode>('office')
  const [query, setQuery] = useState('')

  const filteredEntries = useMemo(() => {
    const q = query.trim().toLowerCase()
    return entries.filter((e) => {
      if (e.mode !== mode) return false
      if (!q) return true
      return (
        e.title.toLowerCase().includes(q) ||
        e.content.toLowerCase().includes(q) ||
        e.tags.some((t) => t.toLowerCase().includes(q))
      )
    })
  }, [entries, mode, query])

  const selectedEntry = entries.find((e) => e.id === selectedId) ?? null

  const handleStartCreate = (): void => {
    setIsCreating(true)
    setSelectedId(null)
  }

  const handleSave = (entry: Omit<KnowledgeEntry, 'id' | 'updatedAt'> & { id?: string }): void => {
    if (entry.id) {
      setEntries((prev) =>
        prev.map((e) => (e.id === entry.id ? { ...e, ...entry, updatedAt: Date.now() } : e))
      )
    } else {
      const newEntry: KnowledgeEntry = {
        ...entry,
        id: `k-${Date.now().toString(36)}`,
        updatedAt: Date.now()
      }
      setEntries((prev) => [newEntry, ...prev])
      setSelectedId(newEntry.id)
    }
    setIsCreating(false)
  }

  const handleDelete = (id: string): void => {
    setEntries((prev) => prev.filter((e) => e.id !== id))
    if (selectedId === id) {
      setSelectedId(null)
    }
  }

  return (
    <div className="flex h-full flex-col overflow-hidden bg-canvas">
      {/* 头部导航与搜索栏 */}
      <div className="shrink-0 border-b border-border/[0.08] px-6 py-5 bg-surface">
        <div className="mx-auto flex max-w-[1200px] flex-col gap-4">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-end sm:justify-between">
            <div>
              <h1 className="font-display text-[22px] font-semibold tracking-tight text-ink">
                知识库与语料规范
              </h1>
              <p className="mt-1 text-[13px] text-ink-muted">
                为模型检索提供专业领域上下文与确定性参考资料
              </p>
            </div>

            <div className="flex items-center gap-2">
              <div className="glass-inset flex items-center gap-2 px-3 py-1.5">
                <Search size={14} className="shrink-0 text-ink-muted" />
                <input
                  type="text"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                  placeholder="检索知识条目与标签…"
                  className="w-[200px] bg-transparent text-[13px] text-ink outline-none placeholder:text-ink-faint sm:w-[240px]"
                />
              </div>

              <button
                type="button"
                onClick={handleStartCreate}
                className="glass-accent-solid flex items-center gap-1.5 px-3.5 py-1.5 text-[12.5px] font-medium"
              >
                <Plus size={14} />
                <span>新建条目</span>
              </button>
            </div>
          </div>

          {/* 模式分段选择器 */}
          <div className="flex items-center gap-1.5">
            {MODES.map((m) => {
              const active = m.id === mode
              const Icon = m.icon
              const count = entries.filter((e) => e.mode === m.id).length
              return (
                <button
                  key={m.id}
                  type="button"
                  onClick={() => {
                    setMode(m.id)
                    setIsCreating(false)
                  }}
                  className={`flex items-center gap-2 rounded-subtle border px-3 py-1.5 text-[12.5px] font-medium transition-colors ${
                    active
                      ? 'border-accent bg-accent/15 text-accent'
                      : 'border-border/[0.1] bg-canvas text-ink-muted hover:text-ink'
                  }`}
                >
                  <Icon size={14} />
                  <span>{m.label}</span>
                  <span className="font-mono text-[12px] opacity-75">({count})</span>
                </button>
              )
            })}
          </div>
        </div>
      </div>

      {/* 主体左右拆分区 */}
      <div className="min-h-0 flex-1 overflow-hidden px-6 py-6">
        <div className="mx-auto flex h-full max-w-[1200px] gap-5">
          <div className="flex w-[320px] shrink-0 flex-col overflow-hidden rounded-panel border border-border/[0.1] bg-surface">
            <EntryList
              entries={filteredEntries}
              selectedId={selectedId}
              onSelect={(id) => {
                setSelectedId(id)
                setIsCreating(false)
              }}
            />
          </div>

          <div className="flex min-w-0 flex-1 flex-col overflow-hidden rounded-panel border border-border/[0.1] bg-surface p-6">
            {isCreating ? (
              <EntryEditor
                mode={mode}
                onSave={handleSave}
                onCancel={() => setIsCreating(false)}
              />
            ) : selectedEntry ? (
              <EntryEditor
                key={selectedEntry.id}
                entry={selectedEntry}
                mode={selectedEntry.mode}
                onSave={handleSave}
                onDelete={() => handleDelete(selectedEntry.id)}
              />
            ) : (
              <div className="flex h-full flex-col items-center justify-center text-center select-none text-ink-muted">
                <Info size={28} className="text-ink-faint mb-2" />
                <p className="text-[14px] font-medium text-ink">请在左侧选择条目，或点击上方新建</p>
                <p className="text-[12.5px] mt-1 text-ink-faint">
                  当前语料包含 {filteredEntries.length} 条已收录的领域规则
                </p>
              </div>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
