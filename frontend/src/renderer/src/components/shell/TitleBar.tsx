import { Minus, Square, X, PanelLeftClose, PanelLeftOpen, Sparkles } from 'lucide-react'
import { useStore } from '../../store/app-store'

export function TitleBar(): React.JSX.Element {
  const sidebarOpen = useStore((s) => s.sidebarOpen)
  const toggleSidebar = useStore((s) => s.toggleSidebar)
  const backend = useStore((s) => s.backend)

  const dotColor =
    backend.kind === 'ready'
      ? 'bg-success'
      : backend.kind === 'error'
        ? 'bg-danger'
        : backend.kind === 'stopped'
          ? 'bg-ink-faint'
          : 'bg-warning'

  return (
    <header className="drag-region flex h-11 shrink-0 items-center gap-2.5 border-b border-border/[0.08] px-3 bg-surface select-none">
      <button
        type="button"
        onClick={toggleSidebar}
        className="no-drag glass-panel glass-panel-hover flex h-7 w-7 items-center justify-center rounded-[6px]"
        title={sidebarOpen ? '收起侧栏' : '展开侧栏'}
      >
        {sidebarOpen ? <PanelLeftClose size={14} /> : <PanelLeftOpen size={14} />}
      </button>

      <div className="flex items-center gap-2 pl-1">
        <Sparkles size={15} className="text-accent" />
        <span className="font-display text-[13.5px] font-semibold tracking-wide text-ink">XimoAgent</span>
      </div>

      <div className="flex-1" />

      <div className="no-drag mr-2 flex items-center gap-2" title={`后端：${backend.kind}`}>
        <span className={`h-2 w-2 rounded-full ${dotColor}`} />
        <span className="text-[12px] font-mono text-ink-muted">
          {backend.kind === 'ready' ? `已连接 · PID ${backend.pid}` : backend.kind}
        </span>
      </div>

      <div className="no-drag flex items-center gap-1">
        <button
          type="button"
          onClick={() => window.ximo.minimizeWindow()}
          className="glass-panel glass-panel-hover flex h-7 w-7 items-center justify-center rounded-[6px]"
          title="最小化"
        >
          <Minus size={13} />
        </button>
        <button
          type="button"
          onClick={() => window.ximo.toggleMaximizeWindow()}
          className="glass-panel glass-panel-hover flex h-7 w-7 items-center justify-center rounded-[6px]"
          title="最大化"
        >
          <Square size={11} />
        </button>
        <button
          type="button"
          onClick={() => window.ximo.closeWindow()}
          className="glass-panel glass-panel-hover flex h-7 w-7 items-center justify-center rounded-[6px] hover:!bg-danger/30 hover:!text-danger"
          title="关闭"
        >
          <X size={13} />
        </button>
      </div>
    </header>
  )
}
