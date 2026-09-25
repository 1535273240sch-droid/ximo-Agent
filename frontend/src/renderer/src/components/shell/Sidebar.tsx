import { Plus, MessageSquare, Trash2, Settings, Users, BookOpen, MessageCircle } from 'lucide-react'
import { useStore } from '../../store/app-store'

const NAV = [
  { id: 'chat', label: '对话', icon: MessageCircle },
  { id: 'experts', label: '专家', icon: Users },
  { id: 'knowledge', label: '知识库', icon: BookOpen },
  { id: 'settings', label: '设置', icon: Settings }
] as const

/**
 * 侧栏：上方是功能导航，下方是会话列表。
 *
 * 极简利落风格：1px 细线边界，清晰中性色阶。
 */
export function Sidebar(): React.JSX.Element {
  const sessions = useStore((s) => s.sessions)
  const activeSessionId = useStore((s) => s.activeSessionId)
  const createSession = useStore((s) => s.createSession)
  const selectSession = useStore((s) => s.selectSession)
  const deleteSession = useStore((s) => s.deleteSession)
  const view = useStore((s) => s.view)
  const setView = useStore((s) => s.setView)

  return (
    <aside className="glass-sidebar flex w-[240px] shrink-0 flex-col">
      <nav className="flex flex-col gap-1 p-2.5">
        {NAV.map(({ id, label, icon: Icon }) => {
          const active = view === id
          return (
            <button
              key={id}
              type="button"
              onClick={() => setView(id)}
              className={[
                'flex items-center gap-2.5 rounded-[8px] px-3 py-2 text-left text-[13px] transition-colors',
                active
                  ? 'glass-raised font-medium text-ink'
                  : 'text-ink-muted hover:bg-surface-raised hover:text-ink'
              ].join(' ')}
            >
              <Icon size={15} className={active ? 'text-accent' : 'text-ink-muted'} />
              <span>{label}</span>
            </button>
          )
        })}
      </nav>

      <div className="mx-2.5 h-px bg-border/[0.08]" />

      <div className="flex items-center justify-between px-3.5 pb-1.5 pt-3">
        <span className="text-[12px] font-semibold tracking-wider text-ink-muted">
          会话历史
        </span>
        <button
          type="button"
          onClick={createSession}
          className="glass-panel glass-panel-hover flex h-6 w-6 items-center justify-center rounded-[6px]"
          title="新建会话"
        >
          <Plus size={13} />
        </button>
      </div>

      <div className="min-h-0 flex-1 overflow-y-auto px-2.5 pb-3">
        {sessions.length === 0 ? (
          <p className="px-2 py-3 text-[12.5px] leading-relaxed text-ink-muted">
            暂无历史会话。新建一个会话开始探索。
          </p>
        ) : (
          <ul className="flex flex-col gap-0.5">
            {sessions.map((s) => {
              const active = s.id === activeSessionId && view === 'chat'
              return (
                <li key={s.id}>
                  <div
                    className={[
                      'group flex items-center gap-2 rounded-[7px] px-2.5 py-2 transition-colors',
                      active ? 'glass-raised font-medium text-ink' : 'hover:bg-surface-raised text-ink-muted'
                    ].join(' ')}
                  >
                    <button
                      type="button"
                      onClick={() => selectSession(s.id)}
                      className="flex min-w-0 flex-1 items-center gap-2 text-left"
                    >
                      <MessageSquare
                        size={13}
                        className={active ? 'shrink-0 text-accent' : 'shrink-0 text-ink-muted'}
                      />
                      <span className="truncate text-[13px]">
                        {s.title}
                      </span>
                    </button>
                    <button
                      type="button"
                      onClick={() => deleteSession(s.id)}
                      className="shrink-0 text-ink-muted opacity-0 transition-opacity hover:text-danger group-hover:opacity-100"
                      title="删除会话"
                    >
                      <Trash2 size={13} />
                    </button>
                  </div>
                </li>
              )
            })}
          </ul>
        )}
      </div>
    </aside>
  )
}
