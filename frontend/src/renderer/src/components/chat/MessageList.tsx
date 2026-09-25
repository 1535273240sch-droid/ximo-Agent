import type { RunView } from '../../store/app-store'
import { MessageBubble } from './MessageBubble'
import { ToolTraceList } from './ToolTraceList'

interface MessageListProps {
  run: RunView
}

/**
 * 消息列表，并把工具轨迹按发生位置插进对话里。
 */
export function MessageList({ run }: MessageListProps): React.JSX.Element {
  const { messages, tools } = run
  const attached = new Set(messages.flatMap((m) => m.toolCalls?.map((t) => t.callId) ?? []))
  const loose = Object.values(tools).filter((t) => !attached.has(t.callId))

  return (
    <div className="flex flex-col gap-5">
      {messages.map((m) => (
        <div key={m.id} className="flex flex-col gap-2">
          {m.role === 'assistant' && m.toolCalls && m.toolCalls.length > 0 && (
            <ToolTraceList traces={m.toolCalls} />
          )}
          <MessageBubble message={m} />
        </div>
      ))}

      {loose.length > 0 && <ToolTraceList traces={loose} />}
    </div>
  )
}
