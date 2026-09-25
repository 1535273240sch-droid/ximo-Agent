import { useEffect, useState } from 'react'
import {
  AlertTriangle,
  CheckCircle2,
  Loader2,
  Plug,
  Plus,
  RefreshCw,
  Trash2,
  Upload
} from 'lucide-react'
import { useStore } from '../../store/app-store'
import type { MCPServerEntryPayload, RuntimeSettingsPayload } from '@shared/types'

/**
 * MCP 服务器配置面板。
 *
 * 读取与写回都走已有的 Settings RPC（window.ximo.getSettings / applySettings）：
 * 后端 ApplyRuntimeSettings 会把 mcp_servers 原样落进 config.json。
 *
 * 与其它设置面板的关键差别：MCP Worker 池在引擎启动时装配（bootstrap 阶段
 * 读取 config 并拉起 MCP 会话），因此保存后**必须重启后端**才生效。面板因此
 * 提供一键重启，而不是让用户以为"保存了却没反应"。
 */
export function McpPanel(): React.JSX.Element {
  const backend = useStore((s) => s.backend)
  const isBackendReady = backend.kind === 'ready'

  const [settings, setSettings] = useState<RuntimeSettingsPayload | null>(null)
  const [servers, setServers] = useState<MCPServerEntryPayload[]>([])
  const [draft, setDraft] = useState<DraftServer>(EMPTY_DRAFT)
  const [showAdd, setShowAdd] = useState(false)
  const [importText, setImportText] = useState('')
  const [showImport, setShowImport] = useState(false)
  const [busy, setBusy] = useState(false)
  const [restarting, setRestarting] = useState(false)
  const [feedback, setFeedback] = useState<{ type: 'success' | 'error'; msg: string } | null>(null)

  useEffect(() => {
    if (!isBackendReady) return
    let cancelled = false
    void window.ximo
      .getSettings()
      .then((s) => {
        if (cancelled) return
        setSettings(s)
        setServers(s.mcp_servers ?? [])
      })
      .catch((err) => {
        if (!cancelled) setFeedback({ type: 'error', msg: `读取 MCP 配置失败：${(err as Error).message}` })
      })
    return () => {
      cancelled = true
    }
  }, [isBackendReady])

  const save = async (next: MCPServerEntryPayload[], okMsg: string): Promise<void> => {
    setBusy(true)
    setFeedback(null)
    try {
      // 先拉最新配置做基底，只覆盖 mcp_servers：本面板挂载后其它面板可能保存过
      // 配置，用挂载时的旧快照整体写回会把那些改动静默回滚。
      const fresh = await window.ximo.getSettings()
      const merged: RuntimeSettingsPayload = { ...fresh, mcp_servers: next }
      await window.ximo.applySettings(merged)
      setSettings(merged)
      setServers(next)
      setFeedback({ type: 'success', msg: okMsg })
    } catch (err) {
      setFeedback({ type: 'error', msg: `保存失败：${(err as Error).message}` })
    } finally {
      setBusy(false)
    }
  }

  const handleAdd = async (): Promise<void> => {
    const entry = draftToEntry(draft)
    if (!entry.name) {
      setFeedback({ type: 'error', msg: '请填写服务器名称' })
      return
    }
    if ((entry.transport ?? 'stdio') === 'stdio' && !entry.command) {
      setFeedback({ type: 'error', msg: 'stdio 传输需要填写启动命令（command）' })
      return
    }
    if ((entry.transport ?? 'stdio') !== 'stdio' && !entry.url) {
      setFeedback({ type: 'error', msg: `${entry.transport} 传输需要填写服务地址（url）` })
      return
    }
    await save([...servers, entry], `已添加「${entry.name}」，重启后端后生效`)
    setDraft(EMPTY_DRAFT)
    setShowAdd(false)
  }

  const handleRemove = async (index: number): Promise<void> => {
    const target = servers[index]
    const next = servers.filter((_, i) => i !== index)
    await save(next, `已移除「${target?.name ?? index}」，重启后端后生效`)
  }

  const handleToggle = async (index: number): Promise<void> => {
    const next = servers.map((s, i) =>
      i === index ? { ...s, enabled: s.enabled === false } : s
    )
    await save(next, '启用状态已更新，重启后端后生效')
  }

  const handleImport = async (): Promise<void> => {
    let imported: MCPServerEntryPayload[]
    try {
      imported = parseImportedServers(importText)
    } catch (err) {
      setFeedback({ type: 'error', msg: `导入解析失败：${(err as Error).message}` })
      return
    }
    // 同名服务器以导入的为准（用户重新粘贴配置通常是修好了旧的）。
    const names = new Set(imported.map((s) => s.name))
    const kept = servers.filter((s) => !s.name || !names.has(s.name))
    await save([...kept, ...imported], `已导入 ${imported.length} 个 MCP 服务器，重启后端后生效`)
    setImportText('')
    setShowImport(false)
  }

  const handleRestart = async (): Promise<void> => {
    setRestarting(true)
    setFeedback(null)
    try {
      await window.ximo.stopBackend()
      await window.ximo.startBackend()
      setFeedback({ type: 'success', msg: '后端已重启，MCP 服务器正在装配' })
    } catch (err) {
      setFeedback({ type: 'error', msg: `重启后端失败：${(err as Error).message}` })
    } finally {
      setRestarting(false)
    }
  }

  if (!isBackendReady) {
    return (
      <div className="flex items-center gap-2 rounded-panel border border-border/[0.1] bg-canvas p-4 text-[12.5px] text-ink-muted">
        <Plug size={15} className="shrink-0 text-warning" />
        <span>Go 后端引擎未就绪。启动后方可配置 MCP 服务器。</span>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-3 text-[13px]">
      <p className="text-[12px] leading-relaxed text-ink-muted">
        支持 stdio（本地进程）与 http / sse（远程服务）两种传输。挂载后，Agent 会通过
        <span className="mx-1 font-medium text-ink">mcp_list_tools</span>
        发现工具，再用
        <span className="mx-1 font-medium text-ink">mcp_call_tool</span>
        调用它们。
      </p>

      {/* 服务器列表 */}
      {servers.length === 0 ? (
        <p className="rounded-panel border border-dashed border-border/[0.15] bg-canvas px-3.5 py-4 text-[12.5px] text-ink-muted">
          尚未挂载任何 MCP 服务器。点击下方「添加」手动配置，或「导入 JSON」粘贴现成配置。
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {servers.map((s, i) => {
            const enabled = s.enabled !== false
            const transport = s.transport ?? (s.url ? 'http' : 'stdio')
            const summary =
              transport === 'stdio'
                ? [s.command, ...(s.args ?? [])].filter(Boolean).join(' ')
                : s.url ?? '(未配置地址)'
            return (
              <div
                key={`${s.name ?? 'server'}-${i}`}
                className="flex items-start gap-3 rounded-panel border border-border/[0.1] bg-canvas px-3.5 py-3"
              >
                <button
                  type="button"
                  onClick={() => void handleToggle(i)}
                  disabled={busy}
                  title={enabled ? '点击停用' : '点击启用'}
                  className={`mt-0.5 shrink-0 rounded-full border px-2 py-0.5 text-[11px] font-medium transition-colors disabled:opacity-60 ${
                    enabled
                      ? 'border-accent/50 bg-accent/12 text-accent'
                      : 'border-border/[0.2] text-ink-muted'
                  }`}
                >
                  {enabled ? '已启用' : '已停用'}
                </button>
                <div className="flex min-w-0 flex-1 flex-col gap-0.5">
                  <span className="flex items-center gap-2">
                    <span className="truncate text-[13px] font-semibold text-ink">
                      {s.name ?? '(未命名)'}
                    </span>
                    <span className="shrink-0 rounded-full border border-border/[0.15] px-2 py-0.5 text-[11px] text-ink-muted">
                      {transport}
                    </span>
                  </span>
                  <span className="truncate font-mono text-[11.5px] text-ink-muted" title={summary}>
                    {summary}
                  </span>
                </div>
                <button
                  type="button"
                  onClick={() => void handleRemove(i)}
                  disabled={busy}
                  title="移除该服务器"
                  className="mt-0.5 shrink-0 rounded-subtle p-1 text-ink-muted transition-colors hover:text-danger disabled:opacity-60"
                >
                  <Trash2 size={14} />
                </button>
              </div>
            )
          })}
        </div>
      )}

      {/* 操作区 */}
      <div className="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onClick={() => setShowAdd((v) => !v)}
          disabled={busy}
          className="flex items-center gap-1.5 rounded-subtle border border-border/[0.15] px-3 py-1.5 text-[12px] text-ink transition-colors hover:border-accent/50 hover:text-accent disabled:opacity-60"
        >
          <Plus size={13} /> 添加服务器
        </button>
        <button
          type="button"
          onClick={() => setShowImport((v) => !v)}
          disabled={busy}
          className="flex items-center gap-1.5 rounded-subtle border border-border/[0.15] px-3 py-1.5 text-[12px] text-ink transition-colors hover:border-accent/50 hover:text-accent disabled:opacity-60"
        >
          <Upload size={13} /> 导入 JSON
        </button>
        <button
          type="button"
          onClick={() => void handleRestart()}
          disabled={restarting}
          className="flex items-center gap-1.5 rounded-subtle border border-border/[0.15] px-3 py-1.5 text-[12px] text-ink transition-colors hover:border-accent/50 hover:text-accent disabled:opacity-60"
        >
          {restarting ? <Loader2 size={13} className="animate-spin" /> : <RefreshCw size={13} />}
          重启后端生效
        </button>
      </div>

      {/* 新增表单 */}
      {showAdd && (
        <div className="flex flex-col gap-2.5 rounded-panel border border-border/[0.12] bg-canvas p-3.5">
          <Field label="名称">
            <input
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              placeholder="例如 filesystem"
              className={INPUT_CLS}
            />
          </Field>
          <Field label="传输方式">
            <select
              value={draft.transport}
              onChange={(e) => setDraft({ ...draft, transport: e.target.value })}
              className={INPUT_CLS}
            >
              <option value="stdio">stdio（本地子进程）</option>
              <option value="http">http</option>
              <option value="sse">sse</option>
            </select>
          </Field>
          {draft.transport === 'stdio' ? (
            <>
              <Field label="启动命令">
                <input
                  value={draft.command}
                  onChange={(e) => setDraft({ ...draft, command: e.target.value })}
                  placeholder="例如 npx 或 C:\\Program Files\\nodejs\\node.exe"
                  className={INPUT_CLS}
                />
              </Field>
              <Field label="参数（每行一个）">
                <textarea
                  value={draft.argsText}
                  onChange={(e) => setDraft({ ...draft, argsText: e.target.value })}
                  rows={3}
                  placeholder={'-y\n@modelcontextprotocol/server-filesystem\nC:\\workspace'}
                  className={`${INPUT_CLS} font-mono`}
                />
              </Field>
            </>
          ) : (
            <Field label="服务地址">
              <input
                value={draft.url}
                onChange={(e) => setDraft({ ...draft, url: e.target.value })}
                placeholder="https://example.com/mcp"
                className={INPUT_CLS}
              />
            </Field>
          )}
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => void handleAdd()}
              disabled={busy}
              className="flex items-center gap-1.5 rounded-subtle border border-accent/50 bg-accent/12 px-3 py-1.5 text-[12px] font-medium text-accent transition-colors disabled:opacity-60"
            >
              {busy ? <Loader2 size={13} className="animate-spin" /> : <CheckCircle2 size={13} />}
              添加
            </button>
            <button
              type="button"
              onClick={() => {
                setDraft(EMPTY_DRAFT)
                setShowAdd(false)
              }}
              className="rounded-subtle px-3 py-1.5 text-[12px] text-ink-muted transition-colors hover:text-ink"
            >
              取消
            </button>
          </div>
        </div>
      )}

      {/* 导入区 */}
      {showImport && (
        <div className="flex flex-col gap-2.5 rounded-panel border border-border/[0.12] bg-canvas p-3.5">
          <p className="text-[12px] leading-relaxed text-ink-muted">
            粘贴 Cursor / Claude Code / Cline 格式的配置（
            <span className="font-mono">{'{ "mcpServers": { ... } }'}</span>
            ）或本系统的服务器数组，可直接从
            <span className="mx-1 font-mono">mcp.json</span>
            复制。
          </p>
          <textarea
            value={importText}
            onChange={(e) => setImportText(e.target.value)}
            rows={7}
            placeholder={'{\n  "mcpServers": {\n    "filesystem": {\n      "command": "npx",\n      "args": ["-y", "@modelcontextprotocol/server-filesystem", "C:\\\\workspace"]\n    }\n  }\n}'}
            className={`${INPUT_CLS} font-mono`}
          />
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => void handleImport()}
              disabled={busy || importText.trim() === ''}
              className="flex items-center gap-1.5 rounded-subtle border border-accent/50 bg-accent/12 px-3 py-1.5 text-[12px] font-medium text-accent transition-colors disabled:opacity-60"
            >
              {busy ? <Loader2 size={13} className="animate-spin" /> : <Upload size={13} />}
              导入
            </button>
            <button
              type="button"
              onClick={() => {
                setImportText('')
                setShowImport(false)
              }}
              className="rounded-subtle px-3 py-1.5 text-[12px] text-ink-muted transition-colors hover:text-ink"
            >
              取消
            </button>
          </div>
        </div>
      )}

      <div className="flex items-start gap-2 rounded-subtle border border-warning/30 bg-warning/10 px-3 py-2.5">
        <AlertTriangle size={14} className="mt-0.5 shrink-0 text-warning" />
        <p className="text-[12px] leading-relaxed text-warning">
          MCP 服务器在引擎启动时装配，配置改动需<span className="font-semibold">重启后端</span>才生效。
          服务器暴露的工具会以
          <span className="mx-1 font-mono">mcp__工具名</span>
          出现在 Agent 的可调用列表中。
        </p>
      </div>

      {feedback && (
        <p
          className={`text-[12px] font-medium ${
            feedback.type === 'success' ? 'text-success' : 'text-danger'
          }`}
        >
          {feedback.msg}
        </p>
      )}

      {settings?.config_path && (
        <p className="truncate text-[11.5px] text-ink-muted" title={settings.config_path}>
          配置文件：<span className="font-mono">{settings.config_path}</span>
        </p>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// 表单辅助
// ---------------------------------------------------------------------------

const INPUT_CLS =
  'w-full rounded-subtle border border-border/[0.15] bg-canvas px-2.5 py-1.5 text-[12.5px] text-ink outline-none transition-colors focus:border-accent/50'

interface DraftServer {
  name: string
  transport: string
  command: string
  argsText: string
  url: string
  enabled: boolean
}

const EMPTY_DRAFT: DraftServer = {
  name: '',
  transport: 'stdio',
  command: '',
  argsText: '',
  url: '',
  enabled: true
}

function Field({ label, children }: { label: string; children: React.ReactNode }): React.JSX.Element {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-[11.5px] font-medium text-ink-muted">{label}</span>
      {children}
    </label>
  )
}

/** 把表单草稿转成后端条目。 */
function draftToEntry(d: DraftServer): MCPServerEntryPayload {
  const entry: MCPServerEntryPayload = { name: d.name.trim(), enabled: d.enabled }
  if (d.transport) entry.transport = d.transport
  if (d.transport === 'stdio') {
    entry.command = d.command.trim()
    const args = d.argsText
      .split('\n')
      .map((s) => s.trim())
      .filter(Boolean)
    if (args.length > 0) entry.args = args
  } else {
    entry.url = d.url.trim()
  }
  return entry
}

/**
 * 解析粘贴的 MCP 配置。
 *
 * 与后端 mcp.ParseConfigs 支持同样的三种形态：
 *  1. { "mcpServers": { "name": {...} } }（Cursor / Claude Code / Cline 格式）
 *  2. { "servers": [ ... ] }
 *  3. 裸数组，或单个服务器对象
 */
function parseImportedServers(text: string): MCPServerEntryPayload[] {
  const parsed: unknown = JSON.parse(text)
  const out: MCPServerEntryPayload[] = []

  const pushRaw = (name: string | undefined, raw: unknown): void => {
    if (raw && typeof raw === 'object' && !Array.isArray(raw)) {
      out.push(rawToEntry(name, raw as Record<string, unknown>))
    }
  }

  if (Array.isArray(parsed)) {
    for (const item of parsed) pushRaw(undefined, item)
  } else if (parsed && typeof parsed === 'object') {
    const obj = parsed as Record<string, unknown>
    const named = obj.mcpServers
    if (named && typeof named === 'object' && !Array.isArray(named)) {
      for (const [name, raw] of Object.entries(named as Record<string, unknown>)) {
        pushRaw(name, raw)
      }
    } else if (Array.isArray(obj.servers)) {
      for (const item of obj.servers) pushRaw(undefined, item)
    } else if ('command' in obj || 'url' in obj) {
      pushRaw(undefined, obj)
    }
  }

  if (out.length === 0) {
    throw new Error('未识别到有效的 MCP 服务器条目（缺 command 或 url）')
  }
  return out
}

/** 把目标客户端格式里的单个服务器条目转成后端条目。 */
function rawToEntry(name: string | undefined, raw: Record<string, unknown>): MCPServerEntryPayload {
  const entry: MCPServerEntryPayload = {}
  const finalName = typeof raw.name === 'string' && raw.name !== '' ? raw.name : name
  if (finalName) entry.name = finalName
  if (typeof raw.transport === 'string') entry.transport = raw.transport
  if (typeof raw.command === 'string') entry.command = raw.command
  if (Array.isArray(raw.args)) entry.args = raw.args.map(String)
  if (raw.env && typeof raw.env === 'object') entry.env = raw.env as Record<string, string>
  if (typeof raw.cwd === 'string') entry.cwd = raw.cwd
  if (typeof raw.url === 'string') entry.url = raw.url
  if (raw.headers && typeof raw.headers === 'object') {
    entry.headers = raw.headers as Record<string, string>
  }
  if (raw.enabled === false) entry.enabled = false
  return entry
}
