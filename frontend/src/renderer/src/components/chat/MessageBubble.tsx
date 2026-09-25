import { useState } from 'react'
import { Brain, ChevronDown, ChevronRight, Sparkles } from 'lucide-react'
import type { ChatMessage } from '../../store/app-store'
import { Markdown } from './Markdown'

interface MessageBubbleProps {
  message: ChatMessage
}

export function MessageBubble({ message }: MessageBubbleProps): React.JSX.Element {
  const isUser = message.role === 'user'

  if (isUser) {
    return (
      <div className="flex justify-end">
        <div className="max-w-[85%] rounded-[10px] border border-border/[0.12] bg-surface-raised px-4 py-3 text-[13.5px] leading-relaxed text-ink shadow-subtle sm:max-w-[75%]">
          <p className="whitespace-pre-wrap select-text">{message.content}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2 text-ink-muted">
        <div className="flex h-5 w-5 items-center justify-center rounded-[5px] border border-border/[0.1] bg-surface text-accent">
          <Sparkles size={11} />
        </div>
        <span className="text-[12px] font-semibold text-ink">XimoAgent</span>
      </div>

      <div className="glass-panel rounded-[10px] p-4 text-[13.5px] shadow-subtle">
        {message.reasoning && <ReasoningBlock reasoning={message.reasoning} />}

        <div className="min-w-0">
          <Markdown content={message.content} />
          {message.streaming && (
            <span
              className="inline-block h-4 w-1.5 translate-y-0.5 bg-accent animate-pulse-soft ml-1"
              aria-label="正在输出"
            />
          )}
        </div>
      </div>
    </div>
  )
}

function ReasoningBlock({ reasoning }: { reasoning: string }): React.JSX.Element {
  const [open, setOpen] = useState(false)

  return (
    <div className="glass-inset mb-3 overflow-hidden rounded-[8px]">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between px-3 py-2 text-left text-[12px] text-ink-muted transition-colors hover:text-ink"
      >
        <div className="flex items-center gap-1.5 font-medium">
          <Brain size={13} className="text-accent" />
          <span>思考过程 (Reasoning Tokens)</span>
        </div>
        {open ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
      </button>

      {open && (
        <div className="border-t border-border/[0.08] px-3.5 py-3 text-[12.5px] leading-relaxed text-ink-muted select-text whitespace-pre-wrap font-mono">
          {reasoning}
        </div>
      )}
    </div>
  )
}
