import { useEffect, useRef, useState } from 'react'
import { AlertTriangle, ArrowDown, CircleAlert, Loader2 } from 'lucide-react'
import { selectRunForSession, useStore } from '../../store/app-store'
import { isTerminalState } from '@shared/types'
import { MessageList } from './MessageList'
import { Composer } from './Composer'
import { EmptyState } from './EmptyState'
import { PlanConfirmCard } from './PlanConfirmCard'

const NEAR_BOTTOM_PX = 80

const RUN_STATE_HINT: Record<string, string> = {
  queued: '已提交，正在等待调度…',
  planning: '正在解析意图并规划执行路径…',
  thinking: '模型正在深度思考…',
  executing: '正在调度工具执行任务…',
  compacting: '正在压缩对话上下文…',
  recovering: '正在从崩溃日志中恢复运行现场…'
}

export function ChatView(): React.JSX.Element {
  const activeSessionId = useStore((s) => s.activeSessionId)
  const runs = useStore((s) => s.runs)
  const submit = useStore((s) => s.submit)
  const confirmPlan = useStore((s) => s.confirmPlan)
  const run = selectRunForSession(runs, activeSessionId)

  const scrollRef = useRef<HTMLDivElement>(null)
  const [showScrollBottom, setShowScrollBottom] = useState(false)
  const pinnedToBottomRef = useRef(true)

  const handleScroll = (): void => {
    const el = scrollRef.current
    if (!el) return
    const distanceToBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    const isNearBottom = distanceToBottom <= NEAR_BOTTOM_PX
    pinnedToBottomRef.current = isNearBottom
    setShowScrollBottom(!isNearBottom)
  }

  const scrollToBottom = (behavior: ScrollBehavior = 'smooth'): void => {
    const el = scrollRef.current
    if (!el) return
    el.scrollTo({ top: el.scrollHeight, behavior })
    pinnedToBottomRef.current = true
    setShowScrollBottom(false)
  }

  useEffect(() => {
    pinnedToBottomRef.current = true
    setShowScrollBottom(false)
    scrollToBottom('auto')
  }, [run?.runId])

  useEffect(() => {
    if (pinnedToBottomRef.current) {
      scrollToBottom('auto')
    }
  }, [run?.messages.length, run?.messages[run?.messages.length - 1]?.content, run?.lastSeq])

  const hasMessages = run && run.messages.length > 0
  const pendingPlan = run?.pendingPlan
  // 任务4：有计划待确认时，等待提示改由计划卡片承担（它本身就说明了要做什么
  // 决定），这里不再显示"正在处理任务…"的转圈条——否则等待用户决定的同时
  // 界面还在说"正在处理"，自相矛盾。
  const isRunning = run && !isTerminalState(run.state) && !pendingPlan
  const isWaitingUser = run?.state === 'waiting_user' && !pendingPlan

  return (
    <div className="flex h-full flex-col overflow-hidden bg-canvas">
      {/* 消息展示区 */}
      <div
        ref={scrollRef}
        onScroll={handleScroll}
        className="relative min-h-0 flex-1 overflow-y-auto px-4 py-6 md:px-8"
      >
        <div className="mx-auto flex max-w-[820px] flex-col gap-6">
          {!hasMessages ? (
            <EmptyState onSelectPrompt={(p) => void submit(p)} />
          ) : (
            <MessageList run={run} />
          )}

          {/* 执行状态指示条 */}
          {isRunning && (
            <div className="flex items-center gap-2.5 rounded-[8px] border border-border/[0.1] bg-surface p-3 text-[12.5px] text-ink-muted">
              <Loader2 size={14} className="animate-spin text-accent" />
              <span className="font-medium text-ink">
                {RUN_STATE_HINT[run.state] ?? '正在处理任务…'}
              </span>
              {run.round > 0 && (
                <span className="font-mono text-[12px] text-ink-faint">
                  (第 {run.round} 轮推理)
                </span>
              )}
            </div>
          )}

          {/* 异常错误横幅 */}
          {run?.error && (
            <div className="flex items-start gap-2.5 rounded-[8px] border border-danger/30 bg-danger/10 p-3.5 text-[13px] text-danger">
              <AlertTriangle size={16} className="mt-0.5 shrink-0" />
              <div className="flex flex-col gap-1">
                <span className="font-semibold text-danger">任务执行异常中断</span>
                <span className="font-mono text-[12px] leading-relaxed opacity-95">
                  {run.error}
                </span>
              </div>
            </div>
          )}

          {/* 计划确认卡片（任务4）：停在等待用户决定的状态 */}
          {pendingPlan && run && (
            <PlanConfirmCard
              plan={pendingPlan}
              onDecide={(approved) => void confirmPlan(run.runId, approved)}
            />
          )}

          {/* 等待人工审批 */}
          {isWaitingUser && (
            <div className="flex items-start gap-2.5 rounded-[8px] border border-warning/30 bg-warning/10 p-3.5 text-[13px] text-warning">
              <CircleAlert size={16} className="mt-0.5 shrink-0" />
              <div className="flex flex-col gap-1">
                <span className="font-semibold text-warning">安全策略阻断：等待操作授权</span>
                <span className="text-[12.5px] text-ink-muted">
                  当前工具调用包含潜在风险操作（高危删除/执行），需要明确批准后方可继续。
                </span>
              </div>
            </div>
          )}
        </div>
      </div>

      {/* 回到底部浮动按钮 */}
      {showScrollBottom && (
        <div className="relative mx-auto w-full max-w-[820px]">
          <button
            type="button"
            onClick={() => scrollToBottom('smooth')}
            className="glass-panel glass-panel-hover absolute -top-12 right-4 flex items-center gap-1.5 px-3 py-1.5 text-[12px] font-medium text-ink shadow-card"
          >
            <ArrowDown size={13} />
            <span>回到最新</span>
          </button>
        </div>
      )}

      {/* 底部输入框 */}
      <div className="shrink-0 border-t border-border/[0.08] px-4 py-3.5 md:px-8 bg-surface">
        <div className="mx-auto max-w-[820px]">
          <Composer />
        </div>
      </div>
    </div>
  )
}
