/**
 * Preload：渲染进程与主进程之间的安全桥。
 *
 * 安全姿态：contextIsolation=true + nodeIntegration=false，渲染层拿不到 require、
 * 拿不到 process、也拿不到 fs。它只能调用这里白名单暴露的方法。
 *
 * 暴露面刻意保持最窄：只有业务动作 + 两个订阅回调，没有任何"执行任意命令"
 * 或"读写任意文件"的能力。这既是 Electron 的安全要求，也让渲染层与后端的
 * 耦合面清晰可控。
 */

import { contextBridge, ipcRenderer } from 'electron'
import { IPC } from '../shared/channels'
import type {
  BackendStatus,
  DecidePayload,
  DecideResultPayload,
  DurableEvent,
  EventsPayload,
  ExpertCardPayload,
  ExpertDeletePayload,
  ExpertListPayload,
  HandlePayload,
  MemoryExport,
  MemoryGraph,
  MemoryGraphMutation,
  MemoryGraphRequest,
  MemoryLink,
  MemoryNodeDetail,
  MemoryNodeUpdate,
  MemoryStats,
  ModelListPayload,
  RecoveryPayload,
  RunPayload,
  RuntimeSettingsPayload,
  SecretStatusPayload,
  SubmitPayload,
  XimoBridge
} from '../shared/types'

const bridge: XimoBridge = {
  startBackend: (): Promise<BackendStatus> => ipcRenderer.invoke(IPC.BackendStart),

  stopBackend: (): Promise<void> => ipcRenderer.invoke(IPC.BackendStop),

  backendStatus: (): Promise<BackendStatus> => ipcRenderer.invoke(IPC.BackendStatus),

  submit: (payload: SubmitPayload): Promise<HandlePayload> =>
    ipcRenderer.invoke(IPC.RunSubmit, payload),

  cancel: (runId: string): Promise<void> => ipcRenderer.invoke(IPC.RunCancel, runId),

  status: (runId: string): Promise<RunPayload> => ipcRenderer.invoke(IPC.RunStatus, runId),

  events: (runId: string, afterSeq: number): Promise<EventsPayload> =>
    ipcRenderer.invoke(IPC.RunEvents, runId, afterSeq),

  resume: (runId?: string): Promise<RecoveryPayload> =>
    ipcRenderer.invoke(IPC.RunResume, runId ?? ''),

  confirmPlan: (
    runId: string,
    approved: boolean
  ): Promise<{ run_id: string; approved: boolean; ok: boolean }> =>
    ipcRenderer.invoke(IPC.PlanConfirm, runId, approved),

  // 工具授权（F5）：批准/拒绝一次待确认的工具调用。载荷字段与后端
  // ipcapi.DecidePayload 一一对应，前端不额外解释它。
  decide: (payload: DecidePayload): Promise<DecideResultPayload> =>
    ipcRenderer.invoke(IPC.RunDecide, payload),

  secretStatus: (): Promise<SecretStatusPayload> => ipcRenderer.invoke(IPC.SecretStatus),

  putSecret: (value: string): Promise<{ ref: string; ok: boolean }> =>
    ipcRenderer.invoke(IPC.SecretPut, value),

  getSettings: (): Promise<RuntimeSettingsPayload> => ipcRenderer.invoke(IPC.ConfigGet),

  applySettings: (payload: RuntimeSettingsPayload): Promise<{ ok: boolean }> =>
    ipcRenderer.invoke(IPC.ConfigSet, payload),

  listModels: (opts?: { base_url?: string }): Promise<ModelListPayload> =>
    ipcRenderer.invoke(IPC.ModelList, opts),

  // ---- 记忆图（P1-c，审核文档 4.9）---------------------------------------
  // 与 confirmPlan / decide 完全同构：一个频道一个 invoke，载荷原样透传。
  // 这里不做字段名转换，也不做默认值填充——两端共用同一份契约。
  memoryGraph: (req: MemoryGraphRequest): Promise<MemoryGraph> =>
    ipcRenderer.invoke(IPC.MemoryGraph, req),

  memoryNode: (id: string): Promise<MemoryNodeDetail> =>
    ipcRenderer.invoke(IPC.MemoryNodeGet, id),

  memoryUpdateNode: (upd: MemoryNodeUpdate): Promise<MemoryNodeDetail> =>
    ipcRenderer.invoke(IPC.MemoryNodeUpdate, upd),

  memoryDeleteNode: (id: string): Promise<void> =>
    ipcRenderer.invoke(IPC.MemoryNodeDelete, id),

  memoryLink: (link: MemoryLink): Promise<MemoryGraphMutation> =>
    ipcRenderer.invoke(IPC.MemoryLink, link),

  memoryConsolidate: (): Promise<MemoryGraphMutation> =>
    ipcRenderer.invoke(IPC.MemoryConsolidate),

  memoryExport: (): Promise<MemoryExport> => ipcRenderer.invoke(IPC.MemoryExport),

  memoryImport: (doc: MemoryExport): Promise<MemoryGraphMutation> =>
    ipcRenderer.invoke(IPC.MemoryImport, doc),

  memoryStats: (): Promise<MemoryStats> => ipcRenderer.invoke(IPC.MemoryStats),

  memoryClear: (confirm: string): Promise<MemoryGraphMutation> =>
    ipcRenderer.invoke(IPC.MemoryClear, confirm),

  // ---- 专家目录（v2.6.0）-------------------------------------------------
  // 与记忆同构：一个频道一个 invoke，载荷原样透传，不做字段名转换。
  expertList: (): Promise<ExpertListPayload> => ipcRenderer.invoke(IPC.ExpertList),

  expertSave: (card: ExpertCardPayload): Promise<ExpertCardPayload> =>
    ipcRenderer.invoke(IPC.ExpertSave, card),

  expertDelete: (id: string): Promise<ExpertDeletePayload> =>
    ipcRenderer.invoke(IPC.ExpertDelete, id),

  onEvent: (handler: (ev: DurableEvent) => void): (() => void) => {
    const listener = (_e: unknown, ev: DurableEvent): void => handler(ev)
    ipcRenderer.on(IPC.EventPushed, listener)
    return () => {
      ipcRenderer.removeListener(IPC.EventPushed, listener)
    }
  },

  onBackendStatus: (handler: (st: BackendStatus) => void): (() => void) => {
    const listener = (_e: unknown, st: BackendStatus): void => handler(st)
    ipcRenderer.on(IPC.BackendStatusChanged, listener)
    return () => {
      ipcRenderer.removeListener(IPC.BackendStatusChanged, listener)
    }
  },

  minimizeWindow: (): void => {
    ipcRenderer.send(IPC.WindowMinimize)
  },

  toggleMaximizeWindow: (): void => {
    ipcRenderer.send(IPC.WindowMaximize)
  },

  closeWindow: (): void => {
    ipcRenderer.send(IPC.WindowClose)
  }
}

contextBridge.exposeInMainWorld('ximo', bridge)
