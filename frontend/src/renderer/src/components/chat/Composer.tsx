import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Bot, CornerDownLeft, SendHorizontal, Square, X } from 'lucide-react'
import { selectRunForSession, useStore } from '../../store/app-store'
import { isTerminalState } from '@shared/types'
import { ModelPicker } from './ModelPicker'
import { GoModeToggle } from './GoModeToggle'
import { PlanModeToggle } from './PlanModeToggle'
import { ExpertQuickPicker } from '../experts/ExpertQuickPicker'

const LINE_HEIGHT_PX = 22
const MAX_ROWS = 8

/**
 * 输入框。模型/GO/计划/专家四项选项提升到了全局 store（见 app-store 的
 * 「输入框选项」）：空会话的「建议探索场景」按钮提交时也要带上同样的开关，
 * 若留在本组件的局部 state，场景提交会绕过计划模式等选项。
 */
export function Composer(): React.JSX.Element {
  const [prompt, setPrompt] = useState('')
  const model = useStore((s) => s.model)
  const setModel = useStore((s) => s.setModel)
  const goMode = useStore((s) => s.goMode)
  const setGoMode = useStore((s) => s.setGoMode)
  const planMode = useStore((s) => s.planMode)
  const setPlanMode = useStore((s) => s.setPlanMode)
  const selectedExpert = useStore((s) => s.selectedExpert)
  const setSelectedExpert = useStore((s) => s.setSelectedExpert)
  const [expertPickerOpen, setExpertPickerOpen] = useState(false)
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const isComposingRef = useRef(false)

  const activeSessionId = useStore((s) => s.activeSessionId)
  const runs = useStore((s) => s.runs)
  const submit = useStore((s) => s.submit)
  const cancelActive = useStore((s) => s.cancelActive)

  const run = selectRunForSession(runs, activeSessionId)
  const isRunning = run && !isTerminalState(run.state)

  useLayoutEffect(() => {
    const el = textareaRef.current
    if (!el) return
    el.style.height = 'auto'
    const maxHeight = LINE_HEIGHT_PX * MAX_ROWS
    const targetHeight = Math.min(el.scrollHeight, maxHeight)
    el.style.height = `${Math.max(LINE_HEIGHT_PX, targetHeight)}px`
    el.style.overflowY = el.scrollHeight > maxHeight ? 'auto' : 'hidden'
  }, [prompt])

  useEffect(() => {
    if (!isRunning) {
      textareaRef.current?.focus()
    }
  }, [isRunning, activeSessionId])

  const handleSend = (): void => {
    const trimmed = prompt.trim()
    if (!trimmed || isRunning) return
    setPrompt('')
    if (textareaRef.current) {
      textareaRef.current.style.height = 'auto'
    }
    // 选项（模型/GO/计划/专家）由 store 的 submit 统一组装进 payload：
    // 场景按钮与手动发送因此走完全相同的规则。专家绑定在提交后自动清空。
    void submit(trimmed)
  }

  const handlePromptChange = (val: string): void => {
    setPrompt(val)
    // 检测输入以 /expert 开头时自动弹出专家选择器
    if (val.trim().startsWith('/expert') && !expertPickerOpen) {
      setExpertPickerOpen(true)
    }
  }

  const handleKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>): void => {
    if (isComposingRef.current || e.nativeEvent.isComposing) {
      return
    }
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      handleSend()
    }
  }

  const hasContent = prompt.trim().length > 0

  return (
    <div className="flex flex-col gap-2">
      <div className="glass-inset relative flex items-end gap-2.5 p-2.5">
        {/* 任务3：专家选择入口，按任务1约定放在输入框左侧（靠文本区），与右侧模型选择器分开 */}
        <div className="relative shrink-0 flex items-center self-end mb-0.5">
          {selectedExpert ? (
            <div
              className="flex h-8 items-center gap-1.5 rounded-[6px] border border-accent/40 bg-accent/15 px-2 text-[12px] font-medium text-accent transition-colors"
              title={`本次任务已绑定专家：${selectedExpert.name} (${selectedExpert.id})`}
            >
              <span className="text-[14px]">{selectedExpert.emoji}</span>
              <span
                onClick={() => setExpertPickerOpen(!expertPickerOpen)}
                className="cursor-pointer max-w-[90px] truncate hover:underline"
              >
                {selectedExpert.name}
              </span>
              <button
                type="button"
                onClick={() => setSelectedExpert(undefined)}
                className="ml-0.5 rounded p-0.5 text-accent/70 hover:bg-accent/20 hover:text-accent transition-colors"
                title="取消绑定专家"
              >
                <X size={12} />
              </button>
            </div>
          ) : (
            <button
              type="button"
              onClick={() => setExpertPickerOpen(!expertPickerOpen)}
              className="flex h-8 items-center gap-1.5 rounded-[6px] border border-border/[0.1] px-2 text-[12px] text-ink-muted hover:bg-surface hover:text-ink transition-colors"
              title="选择本次执行的 AI 专家（输入 /expert 亦可快速呼出）"
            >
              <Bot size={13} className="shrink-0 text-accent" />
              <span className="text-[12px]">选专家</span>
            </button>
          )}

          {/* 专家快捷选择弹窗 */}
          {expertPickerOpen && (
            <ExpertQuickPicker
              inputText={prompt}
              selectedExpertId={selectedExpert?.id}
              onSelect={(exp) => {
                setSelectedExpert(exp)
                // 专家自带内部方案阶段，后端会忽略 plan_mode；选中专家时
                // 同步关掉计划模式，避免用户误以为提交后还会等计划确认。
                if (exp) setPlanMode(false)
                // 若用户以 /expert 开头，移除该前缀避免污染正文
                if (exp && prompt.trim().startsWith('/expert')) {
                  setPrompt((prev) => prev.replace(/^\s*\/expert\s*/, ''))
                }
              }}
              onClose={() => setExpertPickerOpen(false)}
            />
          )}
        </div>

        <textarea
          ref={textareaRef}
          value={prompt}
          onChange={(e) => handlePromptChange(e.target.value)}
          onKeyDown={handleKeyDown}
          onCompositionStart={() => {
            isComposingRef.current = true
          }}
          onCompositionEnd={() => {
            isComposingRef.current = false
          }}
          placeholder="向 XimoAgent 下达任务指令… (Shift + Enter 换行，Enter 直接发送；输入 /expert 快速选专家)"
          rows={1}
          style={{ lineHeight: `${LINE_HEIGHT_PX}px` }}
          className="max-h-[176px] min-h-[22px] w-full resize-none bg-transparent px-1 font-ui text-[13.5px] text-ink outline-none placeholder:text-ink-faint"
        />

        <div className="flex shrink-0 items-center gap-1.5">
          <ModelPicker value={model} onChange={setModel} />
          <GoModeToggle value={goMode} onChange={setGoMode} />
          <PlanModeToggle
            value={planMode}
            onChange={setPlanMode}
            disabled={selectedExpert !== undefined}
            disabledTitle={
              selectedExpert
                ? `已选择专家「${selectedExpert.name}」：专家自带方案阶段，无需计划确认`
                : undefined
            }
          />
          {isRunning ? (
            <button
              type="button"
              onClick={() => void cancelActive()}
              className="flex h-8 items-center gap-1.5 rounded-[6px] border border-danger/40 bg-danger/15 px-3 text-[12px] font-semibold text-danger transition-colors hover:bg-danger/25"
              title="中断当前任务"
            >
              <Square size={12} className="fill-current" />
              <span>停止</span>
            </button>
          ) : (
            <button
              type="button"
              onClick={handleSend}
              disabled={!hasContent}
              className={[
                'flex h-8 w-8 items-center justify-center rounded-[6px] transition-all',
                hasContent
                  ? 'glass-accent-solid'
                  : 'glass-inset cursor-not-allowed text-ink-faint opacity-50'
              ].join(' ')}
              title={hasContent ? '发送指令' : '请输入指令'}
            >
              <SendHorizontal size={14} />
            </button>
          )}
        </div>
      </div>

      <div className="flex items-center justify-between px-1 text-[12px] text-ink-muted">
        <div className="flex items-center gap-1.5">
          <CornerDownLeft size={11} className="text-ink-faint" />
          <span>Enter 确认执行</span>
          <span className="text-ink-faint">·</span>
          <span>Shift+Enter 换行</span>
          <span className="text-ink-faint">·</span>
          <span>/expert 选专家</span>
        </div>
        <span className="font-mono text-[12px] text-ink-faint">第三方 OpenAI 兼容接口</span>
      </div>
    </div>
  )
}

