import { AlertTriangle, Loader2, Plug, PlugZap } from 'lucide-react'
import { useStore, selectRunForSession } from '../../store/app-store'
import { isTerminalState } from '@shared/types'

const STATE_LABEL: Record<string, string> = {
  created: '已创建',
  queued: '排队中',
  planning: '规划中',
  thinking: '思考中',
  executing: '执行中',
  compacting: '压缩上下文',
  waiting_user: '等待确认',
  completed: '已完成',
  cancelled: '已取消',
  failed: '失败',
  recovering: '恢复中'
}

export function StatusBar(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const activeSessionId = useStore((s) => s.activeSessionId)
  const runs = useStore((s) => s.runs)
  const startBackend = useStore((s) => s.setBackend)

  const run = selectRunForSession(runs, activeSessionId)

  const retry = (): void => {
    void window.ximo.startBackend().then(startBackend)
  }

  return (
    <footer className="flex h-9 shrink-0 items-center gap-3 border-t border-border/[0.08] px-3.5 text-[12px] bg-surface">
      {/* 后端连接 */}
      <div className="flex items-center gap-1.5">
        {backend.kind === 'ready' ? (
          <>
            <PlugZap size={13} className="text-success" />
            <span className="text-ink-muted">后端已连接</span>
          </>
        ) : backend.kind === 'error' ? (
          <>
            <AlertTriangle size={13} className="text-danger" />
            <span className="max-w-[420px] truncate text-danger" title={backend.message}>
              {backend.message}
            </span>
            <button
              type="button"
              onClick={retry}
              className="glass-panel glass-panel-hover ml-1 px-2.5 py-0.5 text-[12px]"
            >
              重试
            </button>
          </>
        ) : backend.kind === 'stopped' ? (
          <>
            <Plug size={13} className="text-ink-faint" />
            <span className="text-ink-muted">后端未启动</span>
            <button
              type="button"
              onClick={retry}
              className="glass-panel glass-panel-hover ml-1 px-2.5 py-0.5 text-[12px]"
            >
              启动
            </button>
          </>
        ) : (
          <>
            <Loader2 size={13} className="animate-spin text-warning" />
            <span className="text-ink-muted">
              {backend.kind === 'starting' ? '正在启动后端…' : '正在连接…'}
            </span>
          </>
        )}
      </div>

      {run && (
        <>
          <span className="text-ink-faint">·</span>
          <div className="flex items-center gap-1.5">
            {!isTerminalState(run.state) && (
              <Loader2 size={12} className="animate-spin text-accent" />
            )}
            <span className="font-medium text-ink">{STATE_LABEL[run.state] ?? run.state}</span>
            {run.round > 0 && <span className="font-mono text-ink-muted">第 {run.round} 轮</span>}
          </div>
        </>
      )}

      <div className="flex-1" />

      <span className="font-mono text-[12px] text-ink-faint">事件序号 {run?.lastSeq ?? 0}</span>
    </footer>
  )
}
