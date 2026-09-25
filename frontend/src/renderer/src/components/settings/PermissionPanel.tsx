import { useEffect, useState } from 'react'
import {
  AlertTriangle,
  Briefcase,
  CheckCircle2,
  Code2,
  Loader2,
  Palette,
  Shield,
  ShieldAlert,
  ShieldCheck
} from 'lucide-react'
import { useStore } from '../../store/app-store'
import type { RuntimeSettingsPayload } from '@shared/types'

/**
 * 权限模式选项。id 与后端 tool.Mode / config.runtime.auto_mode 的取值一致：
 * yolo | safe | coding | office | design。
 *
 * 后端权限引擎（internal/tool/permission.go）按模式决定工具调用的
 * 放行/询问/拒绝策略，这里只负责把开关暴露给用户，不参与判定。
 */
const PERMISSION_MODES = [
  {
    id: 'yolo',
    name: 'YOLO 完全放权',
    icon: ShieldAlert,
    desc: '所有工具调用不再询问、直接执行，包括删除文件、执行命令、操作浏览器等高危操作。',
    risky: true
  },
  {
    id: 'safe',
    name: '安全模式',
    icon: ShieldCheck,
    desc: '常规只读操作自动放行；删除文件、执行命令等高危操作需确认；界面自动化类操作被禁止。',
    risky: false
  },
  {
    id: 'coding',
    name: '编程模式',
    icon: Code2,
    desc: '文件读写、代码检索自动放行；执行命令、git 写操作、删除文件需确认；SSH 密钥与 .env 等敏感路径拒绝访问。',
    risky: false
  },
  {
    id: 'office',
    name: '办公模式',
    icon: Briefcase,
    desc: '联网搜索、文件阅读自动放行；写入文件、执行命令等改动类操作一律先询问。',
    risky: false
  },
  {
    id: 'design',
    name: '设计模式',
    icon: Palette,
    desc: '面向设计创作场景。当前版本引擎未单独配置该档，权限规则与编程模式一致。',
    risky: false
  }
] as const

type PermissionModeId = (typeof PERMISSION_MODES)[number]['id']

/**
 * 权限模式开关面板（任务2 Part B）。
 *
 * 读取与写回都走已有的 Settings RPC（window.ximo.getSettings / applySettings），
 * 后端 ApplyRuntimeSettings 只更新配置并落盘 config.json：运行中的引擎进程
 * 仍按启动时的模式执行，重启应用后新的权限模式才生效。
 *
 * 注意：applySettings 提交的是整份 RuntimeSettingsPayload，必须先读全量快照、
 * 只改 auto_mode 字段后原样写回，不能只发单个字段（会把其他配置清空）。
 */
export function PermissionPanel(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const isBackendReady = backend.kind === 'ready'

  const [settings, setSettings] = useState<RuntimeSettingsPayload | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [savingMode, setSavingMode] = useState<PermissionModeId | null>(null)
  const [feedback, setFeedback] = useState<{ type: 'success' | 'error'; msg: string } | null>(null)

  useEffect(() => {
    if (!isBackendReady) return
    let cancelled = false
    void window.ximo
      .getSettings()
      .then((s) => {
        if (!cancelled) {
          setSettings(s)
          setLoadError(null)
        }
      })
      .catch((err) => {
        if (!cancelled) setLoadError((err as Error).message)
      })
    return () => {
      cancelled = true
    }
  }, [isBackendReady])

  const currentMode = settings
    ? PERMISSION_MODES.find((m) => m.id === settings.auto_mode)?.id ?? null
    : null

  const handleSelect = async (mode: PermissionModeId): Promise<void> => {
    if (!settings || savingMode || currentMode === mode) return
    const snapshot = settings
    setSavingMode(mode)
    setFeedback(null)
    try {
      // 写回前先拉最新配置做基底，仅覆盖 auto_mode（providers / sub_agent 等
      // 字段原样带上）：本面板挂载后其它设置面板可能保存过配置，若用挂载时的
      // 旧快照整体写回，会把那些改动静默回滚（同 SubAgentModelPanel 的做法）。
      const fresh = await window.ximo.getSettings()
      const merged: RuntimeSettingsPayload = { ...fresh, auto_mode: mode }
      await window.ximo.applySettings(merged)
      setSettings(merged)
      setFeedback({ type: 'success', msg: `权限模式已切换为「${mode}」并写入 config.json` })
    } catch (err) {
      setSettings(snapshot)
      setFeedback({ type: 'error', msg: `切换失败：${(err as Error).message}` })
    } finally {
      setSavingMode(null)
    }
  }

  if (!isBackendReady) {
    return (
      <div className="flex items-center gap-2 rounded-panel border border-border/[0.1] bg-canvas p-4 text-[12.5px] text-ink-muted">
        <Shield size={15} className="text-warning shrink-0" />
        <span>Go 后端引擎未就绪。启动后方可调整权限模式。</span>
      </div>
    )
  }

  const activeMode = PERMISSION_MODES.find((m) => m.id === currentMode)

  return (
    <div className="flex flex-col gap-3 text-[13px]">
      {loadError && (
        <p className="rounded-subtle border border-warning/30 bg-warning/10 px-3 py-2 text-[12px] leading-relaxed text-warning">
          权限模式读取失败（{loadError}）。引擎重启后会恢复上次保存的配置。
        </p>
      )}

      <div className="flex flex-col gap-2">
        {PERMISSION_MODES.map((mode) => {
          const Icon = mode.icon
          const selected = currentMode === mode.id
          const busy = savingMode === mode.id
          // yolo 选中态使用危险色，与其它档位的 accent 色拉开差距，避免误触。
          const selectedCls = mode.risky
            ? 'border-danger/60 bg-danger/12 text-danger'
            : 'border-accent/50 bg-accent/12 text-accent'
          return (
            <button
              key={mode.id}
              type="button"
              onClick={() => void handleSelect(mode.id)}
              disabled={savingMode !== null}
              className={`flex w-full items-start gap-3 rounded-panel border px-3.5 py-3 text-left transition-colors disabled:opacity-60 ${
                selected ? selectedCls : 'border-border/[0.1] bg-canvas hover:border-border/[0.2]'
              }`}
            >
              <Icon size={16} className={`mt-0.5 shrink-0 ${selected ? '' : 'text-ink-muted'}`} />
              <span className="flex min-w-0 flex-col gap-0.5">
                <span className="flex items-center gap-2">
                  <span
                    className={`text-[13px] font-semibold ${selected ? '' : 'text-ink'}`}
                  >
                    {mode.name}
                  </span>
                  {mode.risky && !selected && (
                    <span className="rounded-full border border-danger/40 bg-danger/10 px-2 py-0.5 text-[11px] font-medium text-danger">
                      高危
                    </span>
                  )}
                  {busy && <Loader2 size={12} className="animate-spin" />}
                  {selected && !busy && <CheckCircle2 size={13} className="shrink-0" />}
                </span>
                <span className="text-[12px] leading-relaxed text-ink-muted">{mode.desc}</span>
              </span>
            </button>
          )
        })}
      </div>

      {activeMode?.risky && (
        <div className="flex items-start gap-2 rounded-subtle border border-danger/40 bg-danger/10 px-3 py-2.5">
          <AlertTriangle size={15} className="mt-0.5 shrink-0 text-danger" />
          <p className="text-[12px] leading-relaxed text-danger">
            <span className="font-semibold">已开启完全放权（YOLO）：</span>
            Agent 接下来的任何工具调用都不会再弹出确认，可能不经询问地修改或删除文件、
            执行任意命令、操控浏览器。请仅在受信任、可回滚的工作区中使用；离开时请切回其他模式。
          </p>
        </div>
      )}

      {!activeMode && settings && settings.auto_mode !== '' && (
        <p className="text-[12px] leading-relaxed text-ink-muted">
          当前配置值 “{settings.auto_mode}” 不在可选列表中，选择上方任意一档即可覆盖。
        </p>
      )}

      {feedback && (
        <p
          className={`text-[12px] font-medium ${
            feedback.type === 'success' ? 'text-success' : 'text-danger'
          }`}
        >
          {feedback.msg}
        </p>
      )}
    </div>
  )
}
