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
  DurableEvent,
  EventsPayload,
  HandlePayload,
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

  secretStatus: (): Promise<SecretStatusPayload> => ipcRenderer.invoke(IPC.SecretStatus),

  putSecret: (value: string): Promise<{ ref: string; ok: boolean }> =>
    ipcRenderer.invoke(IPC.SecretPut, value),

  getSettings: (): Promise<RuntimeSettingsPayload> => ipcRenderer.invoke(IPC.ConfigGet),

  applySettings: (payload: RuntimeSettingsPayload): Promise<{ ok: boolean }> =>
    ipcRenderer.invoke(IPC.ConfigSet, payload),

  listModels: (opts?: { base_url?: string }): Promise<ModelListPayload> =>
    ipcRenderer.invoke(IPC.ModelList, opts),

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
