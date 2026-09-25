import { useState } from 'react'
import { AlertTriangle, Plug, PlugZap, RotateCw, Terminal } from 'lucide-react'
import type { BackendStatus } from '@shared/types'
import { useStore } from '../../store/app-store'

interface StatusView {
  label: string
  color: string
  desc: string
}

const STATUS_VIEWS: Record<BackendStatus['kind'], StatusView> = {
  ready: {
    label: '运行中',
    color: 'text-success',
    desc: 'Go 引擎守护进程处于健康活跃状态，已就绪接收并分派任务。'
  },
  starting: {
    label: '正在启动',
    color: 'text-warning',
    desc: '应用正在尝试拉起本地 ximo-agent.exe 进程…'
  },
  connecting: {
    label: '正在连接',
    color: 'text-warning',
    desc: '已检测到守护进程，正在握手并订阅全双工 IPC 二进制事件流…'
  },
  stopped: {
    label: '已停止',
    color: 'text-ink-muted',
    desc: '本地引擎未启动。请点击右侧按钮拉起引擎。'
  },
  error: {
    label: '异常中断',
    color: 'text-danger',
    desc: '后端进程发生故障或未找到二进制执行文件。'
  }
}

export function BackendPanel(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const setBackend = useStore((s) => s.setBackend)
  const [restarting, setRestarting] = useState(false)

  const view = STATUS_VIEWS[backend.kind]

  const handleRestart = async (): Promise<void> => {
    setRestarting(true)
    try {
      await window.ximo.stopBackend()
      const next = await window.ximo.startBackend()
      setBackend(next)
    } finally {
      setRestarting(false)
    }
  }

  return (
    <div className="flex flex-col gap-4 text-[13px]">
      <div className="flex items-center justify-between rounded-[8px] border border-border/[0.08] bg-canvas p-3.5">
        <div className="flex items-center gap-3">
          <div className="flex h-8 w-8 items-center justify-center rounded-[6px] border border-border/[0.1] bg-surface text-accent">
            {backend.kind === 'ready' ? (
              <PlugZap size={16} className="text-success" />
            ) : backend.kind === 'error' ? (
              <AlertTriangle size={16} className="text-danger" />
            ) : (
              <Plug size={16} className="text-ink-muted" />
            )}
          </div>
          <div className="flex flex-col">
            <div className="flex items-center gap-2">
              <span className="font-semibold text-ink">本地进程状态:</span>
              <span className={`font-semibold ${view.color}`}>{view.label}</span>
              {backend.kind === 'ready' && (
                <span className="font-mono text-[12px] text-ink-muted">
                  (PID {backend.pid})
                </span>
              )}
            </div>
            {backend.kind === 'ready' && (
              <span className="font-mono text-[12px] text-ink-faint">
                {backend.endpoint}
              </span>
            )}
          </div>
        </div>

        <button
          type="button"
          onClick={() => void handleRestart()}
          disabled={restarting}
          className="glass-panel glass-panel-hover flex items-center gap-1.5 px-3 py-1.5 text-[12px] font-medium text-ink transition-colors disabled:opacity-50"
        >
          <RotateCw size={13} className={restarting ? 'animate-spin' : ''} />
          <span>{restarting ? '正在重启…' : '重启后端进程'}</span>
        </button>
      </div>

      <p className="text-[12.5px] leading-relaxed text-ink-muted">
        {view.desc}
      </p>

      {backend.kind === 'error' && (
        <div className="rounded-[8px] border border-danger/30 bg-danger/10 p-3 text-[12.5px] text-danger">
          <div className="font-semibold mb-1">错误诊断信息:</div>
          <pre className="whitespace-pre-wrap font-mono text-[12px]">
            {backend.message}
          </pre>
        </div>
      )}

      <div className="flex items-start gap-2.5 rounded-[8px] border border-border/[0.08] bg-surface p-3 text-[12.5px] text-ink-muted">
        <Terminal size={15} className="mt-0.5 shrink-0 text-accent" />
        <div className="flex flex-col gap-1">
          <span className="font-medium text-ink">二进制路径提示</span>
          <span>
            若提示找不到可执行文件，请在仓库根目录执行 <code className="font-mono text-[12px] bg-canvas px-1.5 py-0.5 rounded border border-border/[0.1]">build.cmd exe</code>。产物将自动生成于 <code className="font-mono text-[12px] bg-canvas px-1.5 py-0.5 rounded border border-border/[0.1]">dist/ximo-agent.exe</code>。
          </span>
        </div>
      </div>
    </div>
  )
}
