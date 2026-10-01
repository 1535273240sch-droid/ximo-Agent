import type { RunView } from '../../store/app-store'
import { MessageBubble } from './MessageBubble'
import { WorkLog } from './worklog/WorkLog'

interface MessageListProps {
  run: RunView
  /** 用户手动开合 Work Log；undefined 表示用户还没动过。 */
  workLogOpen?: boolean
  onToggleWorkLog: (open: boolean) => void
  /** 批准/拒绝一次待授权工具调用。 */
  onDecideTool: (callId: string, approve: boolean) => void
  /** 闭环「继续」：以未通过检查项作为新任务继续。 */
  onContinue?: () => void
  continuing?: boolean
  /** 点击「回忆到的某条记忆」→ 跳到记忆页并聚焦该节点。 */
  onOpenMemoryNode?: (nodeId: string) => void
}

/**
 * 消息列表 + 本次任务的 Work Log。
 *
 * 与旧实现的关键区别：以前工具轨迹用「tools 字典 + loose 渲染」拼，attached 集合
 * 恒为空（ChatMessage.toolCalls 从不赋值），所有工具都被统一渲染在全部消息之后，
 * 与对应的文字说明脱节（C6）。现在工具是时间线里的步骤，按发生顺序排列，并且
 * 复核纠偏 / 长任务续段 / 上下文压缩 / 等待授权都在同一条时间线上（C7）。
 *
 * 为什么放在消息列表上方而不是逐条插入：一次 run 的事件序列里，"第 N 轮回答"
 * 与"第 N 轮的工具调用"是同一轮的两面，逐条插入需要把事件重新切分回消息边界，
 * 而那正是旧实现算错的地方。整轮时间线放在这次回答的最上方，位置稳定、顺序
 * 正确，且与审核文档 2.1「放在这次回答的正上方」一致。
 */
export function MessageList({
  run,
  workLogOpen,
  onToggleWorkLog,
  onDecideTool,
  onContinue,
  continuing,
  onOpenMemoryNode
}: MessageListProps): React.JSX.Element {
  const { messages } = run

  return (
    <div className="flex flex-col gap-5">
      <WorkLog
        steps={run.steps ?? []}
        state={run.state}
        closure={run.closure}
        modelUsed={run.modelUsed}
        startedAt={run.startedAt}
        open={workLogOpen}
        onToggleOpen={onToggleWorkLog}
        pendingApproval={run.pendingApproval}
        onDecide={onDecideTool}
        onContinue={onContinue}
        continuing={continuing}
        onOpenMemoryNode={onOpenMemoryNode}
      />

      {messages.map((m) => (
        <div key={m.id} className="flex flex-col gap-2">
          <MessageBubble message={m} />
        </div>
      ))}
    </div>
  )
}
