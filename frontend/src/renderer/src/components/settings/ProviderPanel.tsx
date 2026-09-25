import { useEffect, useState } from 'react'
import {
  CheckCircle2,
  AlertCircle,
  Server,
  Save,
  Loader2,
  ShieldCheck,
  RefreshCw,
  ChevronDown
} from 'lucide-react'
import { useStore } from '../../store/app-store'
import { SecretField } from './SecretField'
import type {
  ModelListPayload,
  RuntimeSettingsPayload,
  SecretStatusPayload
} from '@shared/types'

const PRESET_PROVIDERS = [
  { id: 'openai', name: 'OpenAI', baseUrl: 'https://api.openai.com/v1', defaultModel: 'gpt-4o' },
  { id: 'custom', name: '自定义兼容接口', baseUrl: '', defaultModel: '' }
]

/**
 * 模型服务商与 API 凭证管理面板。
 *
 * 架构特点：
 *  1. 凭证零外泄：明文密钥由操作系统级凭据管理器 (Windows DPAPI / Keychain) 加密，
 *     配置文件与日志中只留存脱敏引用。前端永远拿不回明文。
 *  2. 运行时热重载：更新服务商或密钥后，Go 后端通过 swappableProvider 原子切换。
 *  3. 模型列表可实时拉取：填完密钥后可点「获取模型」向服务商查询可用模型并下拉选择，
 *     避免手敲模型名导致的含糊 400 错误。
 */
export function ProviderPanel(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const isBackendReady = backend.kind === 'ready'

  const [secretStatus, setSecretStatus] = useState<SecretStatusPayload | null>(null)
  const [keyInput, setKeyInput] = useState('')
  const [savingKey, setSavingKey] = useState(false)
  const [keyFeedback, setKeyFeedback] = useState<{ type: 'success' | 'error'; msg: string } | null>(
    null
  )

  const [settings, setSettings] = useState<RuntimeSettingsPayload | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [savingSettings, setSavingSettings] = useState(false)
  const [settingsFeedback, setSettingsFeedback] = useState<{
    type: 'success' | 'error'
    msg: string
  } | null>(null)

  const [models, setModels] = useState<ModelListPayload | null>(null)
  const [loadingModels, setLoadingModels] = useState(false)
  const [modelPickerOpen, setModelPickerOpen] = useState(false)

  const refreshData = async (): Promise<void> => {
    if (!isBackendReady) return
    // 两项查询互相独立：一项失败不能连累另一项。
    const [sec, set] = await Promise.allSettled([
      window.ximo.secretStatus(),
      window.ximo.getSettings()
    ])
    if (sec.status === 'fulfilled') setSecretStatus(sec.value)
    if (set.status === 'fulfilled') {
      setSettings(set.value)
      setLoadError(null)
    } else {
      // 读取失败也要把配置表单渲染出来（填一份中性兜底值）：
      // 整块消失会让界面看起来"只剩密钥框"，用户无从下手。
      const reason = set.reason instanceof Error ? set.reason.message : String(set.reason)
      setSettings((prev) =>
        prev ?? {
          provider_id: 'custom',
          provider_name: '',
          base_url: '',
          model: '',
          context_window: 131072,
          max_output_tokens: 8192,
          secret_ref: '',
          auto_mode: '',
          workspace_root: '',
          db_path: '',
          config_path: ''
        }
      )
      setLoadError(reason)
    }
  }

  useEffect(() => {
    void refreshData()
  }, [isBackendReady])

  const handleSaveKey = async (): Promise<void> => {
    if (!keyInput.trim() || savingKey) return
    setSavingKey(true)
    setKeyFeedback(null)
    try {
      const res = await window.ximo.putSecret(keyInput.trim())
      setKeyInput('')
      setKeyFeedback({ type: 'success', msg: `密钥已加密存入系统凭证库（引用: ${res.ref}）` })
      await refreshData()
      // 密钥就位后顺手拉一次模型列表，省掉用户再点一次。
      void handleFetchModels()
    } catch (err) {
      setKeyFeedback({ type: 'error', msg: `保存密钥失败：${(err as Error).message}` })
    } finally {
      setSavingKey(false)
    }
  }

  const handleFetchModels = async (): Promise<void> => {
    if (loadingModels) return
    setLoadingModels(true)
    setModels(null)
    try {
      // 带上表单里当前填写的地址：用户可能刚改了 URL 还没保存，如果让后端
      // 查已保存的旧地址，报错会让人摸不着头脑（改了也没生效的错觉）。
      const res = await window.ximo.listModels({ base_url: settings?.base_url ?? '' })
      setModels(res)
      if (res.error) {
        setModelPickerOpen(false)
      }
    } catch (err) {
      setModels({ models: null, base_url: '', error: (err as Error).message })
    } finally {
      setLoadingModels(false)
    }
  }

  const handleSaveSettings = async (): Promise<void> => {
    if (!settings || savingSettings) return
    setSavingSettings(true)
    setSettingsFeedback(null)
    try {
      await window.ximo.applySettings(settings)
      setSettingsFeedback({ type: 'success', msg: '服务商配置已热重载并写入 config.json' })
      await refreshData()
    } catch (err) {
      setSettingsFeedback({ type: 'error', msg: `更新配置失败：${(err as Error).message}` })
    } finally {
      setSavingSettings(false)
    }
  }

  const handleSelectPreset = (presetId: string): void => {
    const p = PRESET_PROVIDERS.find((item) => item.id === presetId)
    if (!p || !settings) return
    setSettings({
      ...settings,
      provider_id: p.id,
      provider_name: p.name,
      base_url: p.baseUrl || settings.base_url,
      model: p.defaultModel || settings.model
    })
  }

  if (!isBackendReady) {
    return (
      <div className="flex items-center gap-2 rounded-panel border border-border/[0.1] bg-canvas p-4 text-[12.5px] text-ink-muted">
        <AlertCircle size={15} className="text-warning shrink-0" />
        <span>Go 后端引擎未就绪。启动后方可配置 API 密钥与服务商。</span>
      </div>
    )
  }

  const modelList = models?.models ?? []
  const hasModels = modelList.length > 0

  return (
    <div className="flex flex-col gap-5 text-[13px]">
      {/* 凭证状态 */}
      <div className="flex flex-wrap items-center justify-between gap-3 rounded-panel border border-border/[0.09] bg-canvas p-3.5">
        <div className="flex items-center gap-2.5">
          <ShieldCheck size={18} className="text-accent shrink-0" />
          <div className="flex flex-col">
            <span className="text-[13px] font-medium text-ink">
              系统凭证库：{secretStatus?.backend || '探测中…'}
            </span>
            <span className="font-mono text-[12px] text-ink-muted">
              引用标识：{secretStatus?.ref || '未绑定'}
            </span>
          </div>
        </div>

        {secretStatus?.configured ? (
          <span className="flex items-center gap-1.5 rounded-full border border-success/30 bg-success/10 px-3 py-1 text-[12px] font-medium text-success">
            <CheckCircle2 size={13} />
            <span>已配置密钥</span>
          </span>
        ) : (
          <span className="flex items-center gap-1.5 rounded-full border border-warning/30 bg-warning/10 px-3 py-1 text-[12px] font-medium text-warning">
            <AlertCircle size={13} />
            <span>尚未配置密钥</span>
          </span>
        )}
      </div>

      {/* 密钥录入 */}
      <SecretField
        label="API 密钥"
        value={keyInput}
        onChange={setKeyInput}
        onSave={handleSaveKey}
        saving={savingKey}
        disabled={secretStatus ? !secretStatus.available : false}
        placeholder="sk-..."
        hint="支持任意 OpenAI 兼容的第三方接口。明文不落盘，仅存于系统加密凭证库。"
      />

      {keyFeedback && (
        <p
          className={`text-[12px] font-medium ${
            keyFeedback.type === 'success' ? 'text-success' : 'text-danger'
          }`}
        >
          {keyFeedback.msg}
        </p>
      )}

      <div className="h-px bg-border/[0.07]" />

      {/* 服务商与模型 */}
      {settings && (
        <div className="flex flex-col gap-4">
          {loadError && (
            <p className="rounded-subtle border border-warning/30 bg-warning/10 px-3 py-2 text-[12px] leading-relaxed text-warning">
              配置读取失败（{loadError}）。下方仍可直接填写，待后端恢复后保存即可生效。
            </p>
          )}
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <Server size={15} className="text-accent" />
              <span className="text-[13.5px] font-semibold text-ink">服务商与模型</span>
            </div>
            <span className="text-[12px] text-ink-muted">修改后即时热重载</span>
          </div>

          {/* 预设 */}
          <div className="flex flex-wrap items-center gap-1.5">
            {PRESET_PROVIDERS.map((p) => (
              <button
                key={p.id}
                type="button"
                onClick={() => handleSelectPreset(p.id)}
                className={`rounded-subtle border px-3 py-1 text-[12.5px] font-medium transition-colors ${
                  settings.provider_id === p.id
                    ? 'border-accent/50 bg-accent/12 text-accent'
                    : 'border-border/[0.1] bg-canvas text-ink-muted hover:text-ink'
                }`}
              >
                {p.name}
              </button>
            ))}
          </div>

          <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2">
            <div className="flex flex-col gap-1.5">
              <label className="text-[12px] font-medium text-ink-muted">接口地址 (Base URL)</label>
              <div className="glass-inset px-3 py-2">
                <input
                  type="text"
                  value={settings.base_url}
                  onChange={(e) => setSettings({ ...settings, base_url: e.target.value })}
                  placeholder="https://api.openai.com/v1"
                  className="w-full bg-transparent font-mono text-[13px] text-ink outline-none placeholder:text-ink-faint"
                />
              </div>
            </div>

            {/* 模型名 + 获取模型 */}
            <div className="flex flex-col gap-1.5">
              <div className="flex items-center justify-between">
                <label className="text-[12px] font-medium text-ink-muted">模型名称</label>
                <button
                  type="button"
                  onClick={() => void handleFetchModels()}
                  disabled={loadingModels}
                  className="flex items-center gap-1 text-[12px] font-medium text-accent transition-opacity hover:opacity-75 disabled:opacity-50"
                >
                  {loadingModels ? (
                    <Loader2 size={12} className="animate-spin" />
                  ) : (
                    <RefreshCw size={12} />
                  )}
                  <span>{loadingModels ? '获取中…' : '获取模型'}</span>
                </button>
              </div>

              <div className="relative">
                <div className="glass-inset flex items-center px-3 py-2">
                  <input
                    type="text"
                    value={settings.model}
                    onChange={(e) => setSettings({ ...settings, model: e.target.value })}
                    placeholder="gpt-4o"
                    className="w-full bg-transparent font-mono text-[13px] text-ink outline-none placeholder:text-ink-faint"
                  />
                  {hasModels && (
                    <button
                      type="button"
                      onClick={() => setModelPickerOpen((v) => !v)}
                      className="shrink-0 text-ink-muted transition-colors hover:text-ink"
                      title="从已获取的模型中选择"
                    >
                      <ChevronDown size={14} />
                    </button>
                  )}
                </div>

                {/* 模型下拉：仅在成功拉到列表后出现 */}
                {modelPickerOpen && hasModels && (
                  <div className="absolute left-0 right-0 top-full z-20 mt-1 max-h-[220px] overflow-y-auto rounded-panel border border-border/[0.12] bg-surface-raised shadow-card">
                    {modelList.map((m) => (
                      <button
                        key={m.id}
                        type="button"
                        onClick={() => {
                          setSettings({ ...settings, model: m.id })
                          setModelPickerOpen(false)
                        }}
                        className="flex w-full items-center justify-between px-3 py-2 text-left font-mono text-[12.5px] text-ink transition-colors hover:bg-surface"
                      >
                        <span className="truncate">{m.id}</span>
                        {m.owned_by && (
                          <span className="shrink-0 pl-2 text-[12px] text-ink-faint">
                            {m.owned_by}
                          </span>
                        )}
                      </button>
                    ))}
                  </div>
                )}
              </div>

              {/* 模型查询反馈 */}
              {models?.error && (
                <p className="text-[12px] leading-relaxed text-warning">{models.error}</p>
              )}
              {hasModels && !models?.error && (
                <p className="text-[12px] text-success">
                  已获取 {modelList.length} 个可用模型，点击右侧箭头选择
                </p>
              )}
            </div>

            <div className="flex flex-col gap-1.5">
              <label className="text-[12px] font-medium text-ink-muted">上下文窗口 (Tokens)</label>
              <div className="glass-inset px-3 py-2">
                <input
                  type="number"
                  value={settings.context_window}
                  onChange={(e) =>
                    setSettings({ ...settings, context_window: Number(e.target.value) })
                  }
                  className="w-full bg-transparent font-mono text-[13px] text-ink outline-none"
                />
              </div>
            </div>

            <div className="flex flex-col gap-1.5">
              <label className="text-[12px] font-medium text-ink-muted">最大输出 (Tokens)</label>
              <div className="glass-inset px-3 py-2">
                <input
                  type="number"
                  value={settings.max_output_tokens}
                  onChange={(e) =>
                    setSettings({ ...settings, max_output_tokens: Number(e.target.value) })
                  }
                  className="w-full bg-transparent font-mono text-[13px] text-ink outline-none"
                />
              </div>
            </div>
          </div>

          <div className="flex items-center justify-between gap-3 pt-1">
            <span
              className="truncate font-mono text-[12px] text-ink-faint"
              title={settings.config_path}
            >
              {settings.config_path}
            </span>

            <button
              type="button"
              onClick={() => void handleSaveSettings()}
              disabled={savingSettings || !settings.base_url.trim()}
              className="glass-accent-solid flex shrink-0 items-center gap-1.5 px-4 py-2 text-[12.5px] font-medium disabled:opacity-40"
            >
              {savingSettings ? <Loader2 size={13} className="animate-spin" /> : <Save size={13} />}
              <span>保存配置</span>
            </button>
          </div>

          {settingsFeedback && (
            <p
              className={`text-[12px] font-medium ${
                settingsFeedback.type === 'success' ? 'text-success' : 'text-danger'
              }`}
            >
              {settingsFeedback.msg}
            </p>
          )}
        </div>
      )}
    </div>
  )
}
