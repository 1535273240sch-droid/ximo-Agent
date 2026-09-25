import { useEffect, useState } from 'react'
import type { KeyboardEvent } from 'react'
import { Eye, EyeOff, KeyRound, Loader2, Lock } from 'lucide-react'

interface SecretFieldProps {
  label: string
  value: string
  onChange: (value: string) => void
  onSave: () => void
  saving: boolean
  /** 安全存储不可用或后端未连接时禁用，不做明文降级。 */
  disabled?: boolean
  placeholder?: string
  hint?: string
}

/**
 * 敏感凭证输入框 (SecretField).
 *
 * 架构约束：
 *  - 严禁预填充或回显后端已有密钥（后端只写不读，仅返回引用）。
 *  - 绝不使用低于 12px 的字体，保证像素级锐利度。
 */
export function SecretField({
  label,
  value,
  onChange,
  onSave,
  saving,
  disabled = false,
  placeholder,
  hint
}: SecretFieldProps): React.JSX.Element {
  const [revealed, setRevealed] = useState(false)
  const canSave = value.trim().length > 0 && !saving && !disabled

  useEffect(() => {
    if (value.length === 0) setRevealed(false)
  }, [value])

  const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>): void => {
    if (e.key !== 'Enter') return
    e.preventDefault()
    if (canSave) onSave()
  }

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-1.5">
          <span className="text-[12.5px] font-semibold text-ink">
            {label}
          </span>
          <Lock size={12} className="shrink-0 text-accent" />
        </div>
        <span className="text-[12px] text-ink-muted">
          加密存储于系统级安全凭证库 (DPAPI)
        </span>
      </div>

      <div className="flex items-center gap-2">
        <div className="glass-inset flex min-w-0 flex-1 items-center gap-2 px-3 py-1.5">
          <KeyRound size={14} className="shrink-0 text-ink-muted" />
          <input
            type={revealed ? 'text' : 'password'}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            onKeyDown={handleKeyDown}
            disabled={disabled}
            placeholder={placeholder}
            autoComplete="off"
            spellCheck={false}
            className="w-full min-w-0 bg-transparent font-mono text-[13px] text-ink outline-none placeholder:text-ink-faint disabled:cursor-not-allowed disabled:text-ink-faint"
          />
          <button
            type="button"
            onClick={() => setRevealed((v) => !v)}
            disabled={disabled || value.length === 0}
            title={revealed ? '隐藏本次输入' : '显示本次输入'}
            aria-label={revealed ? '隐藏本次输入' : '显示本次输入'}
            className="shrink-0 text-ink-muted transition-colors hover:text-ink disabled:cursor-not-allowed disabled:opacity-40"
          >
            {revealed ? <EyeOff size={14} /> : <Eye size={14} />}
          </button>
        </div>

        <button
          type="button"
          onClick={onSave}
          disabled={!canSave}
          className={[
            'flex shrink-0 items-center gap-1.5 px-3.5 py-2 text-[12.5px] font-medium transition-all rounded-subtle',
            canSave ? 'glass-accent-solid' : 'glass-inset cursor-not-allowed text-ink-faint opacity-60'
          ].join(' ')}
          title={!value.trim() ? '请先输入密钥' : '写入安全存储'}
        >
          {saving ? <Loader2 size={13} className="animate-spin" /> : <Lock size={13} />}
          <span>{saving ? '写入中…' : '保存密钥'}</span>
        </button>
      </div>

      {hint && <p className="text-[12px] leading-relaxed text-ink-muted">{hint}</p>}
    </div>
  )
}
