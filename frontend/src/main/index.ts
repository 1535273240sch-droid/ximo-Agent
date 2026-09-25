/**
 * Electron 主进程入口。
 *
 * 职责边界：
 *   主进程 = 后端进程管理者 + 帧协议客户端 + 渲染进程的安全网关。
 *   渲染进程 = 纯 UI，不碰网络、不碰子进程，只通过 preload 暴露的窄接口调用。
 *
 * 这是 Electron 的标准安全姿态：contextIsolation 开启、nodeIntegration 关闭，
 * 渲染层拿到的是一个白名单对象，而不是 Node 能力。
 */

import { app, BrowserWindow, ipcMain, shell } from 'electron'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { BackendClient } from './backend-client'
import { BackendManager } from './backend-manager'
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
  SubmitPayload
} from '../shared/types'

const __dirname_ = path.dirname(fileURLToPath(import.meta.url))

const backend = new BackendManager()
let client: BackendClient | null = null
let mainWindow: BrowserWindow | null = null

/** 后端状态变化的广播目标。 */
function broadcastStatus(status: BackendStatus): void {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send(IPC.BackendStatusChanged, status)
  }
}

/** 事件推送的广播目标。 */
function broadcastEvent(ev: DurableEvent): void {
  if (mainWindow && !mainWindow.isDestroyed()) {
    mainWindow.webContents.send(IPC.EventPushed, ev)
  }
}

/**
 * 确保后端与帧连接都可用。
 *
 * 先起进程、再连帧通道、最后等连接就绪。
 *
 * 重试是必需的，不是保险：Supervisor 的监听端口先就绪，而它的 Engine 子进程
 * 还要再花一两秒完成装配（打开 SQLite、跑迁移、构造 Provider）。在那个窗口期
 * 发探活会失败，若直接判定为 error，界面会闪一个"连接失败"再自己变好——
 * 用户看到的是莫名其妙的报错。这里改成带退避的重试，只有真正超时才报错。
 */
async function ensureBackend(): Promise<BackendStatus> {
  const status = await backend.start()
  if (status.kind !== 'ready') return status

  if (!client) {
    const endpoint = backend.getEndpoint()
    if (!endpoint) {
      return { kind: 'error', message: 'backend started but exposed no endpoint' }
    }
    const m = /^tcp:\/\/([^:]+):(\d+)$/.exec(endpoint)
    if (!m) {
      return { kind: 'error', message: `unsupported backend endpoint: ${endpoint}` }
    }
    client = new BackendClient(m[1], Number(m[2]))
    client.onEvent(broadcastEvent)
    client.onStateChange((s) => {
      if (s === 'closed') {
        broadcastStatus({ kind: 'stopped' })
      } else if (s === 'reconnecting') {
        broadcastStatus({ kind: 'connecting' })
      }
    })
  }

  const deadline = Date.now() + 30_000
  let lastErr = ''
  // 首次连接失败后不要立刻重连同一条链路：连接可能已在重连退避里，
  // 这里只需要按同样的节奏再探几次。
  while (Date.now() < deadline) {
    try {
      await client.connect()
      await client.ping()
      return backend.getStatus()
    } catch (err) {
      lastErr = (err as Error).message
      await new Promise((r) => setTimeout(r, 500))
    }
  }
  return { kind: 'error', message: `cannot talk to backend: ${lastErr}` }
}

/** 统一处理需要"后端已连接"的调用。 */
function requireClient(): BackendClient {
  if (!client) {
    throw new Error('backend is not connected yet')
  }
  return client
}

function registerIpcHandlers(): void {
  ipcMain.handle(IPC.BackendStart, async (): Promise<BackendStatus> => {
    const st = await ensureBackend()
    broadcastStatus(st)
    return st
  })

  ipcMain.handle(IPC.BackendStop, async (): Promise<void> => {
    client?.close()
    client = null
    await backend.stop()
    broadcastStatus({ kind: 'stopped' })
  })

  ipcMain.handle(IPC.BackendStatus, async (): Promise<BackendStatus> => backend.getStatus())

  ipcMain.handle(IPC.RunSubmit, async (_e, payload: SubmitPayload): Promise<HandlePayload> => {
    return (await requireClient().submit(payload)) as HandlePayload
  })

  ipcMain.handle(IPC.RunCancel, async (_e, runId: string): Promise<void> => {
    await requireClient().cancel(runId)
  })

  ipcMain.handle(IPC.RunStatus, async (_e, runId: string): Promise<RunPayload> => {
    return (await requireClient().status(runId)) as RunPayload
  })

  ipcMain.handle(
    IPC.RunEvents,
    async (_e, runId: string, afterSeq: number): Promise<EventsPayload> => {
      return (await requireClient().events(runId, afterSeq)) as EventsPayload
    }
  )

  ipcMain.handle(IPC.RunResume, async (_e, runId: string): Promise<RecoveryPayload> => {
    return (await requireClient().resume(runId)) as RecoveryPayload
  })

  // 计划模式（任务4）：确认/否决执行计划。
  ipcMain.handle(
    IPC.PlanConfirm,
    async (_e, runId: string, approved: boolean): Promise<{ run_id: string; approved: boolean; ok: boolean }> => {
      return (await requireClient().confirmPlan(runId, approved)) as {
        run_id: string
        approved: boolean
        ok: boolean
      }
    }
  )

  ipcMain.handle(IPC.SecretStatus, async (): Promise<SecretStatusPayload> => {
    return (await requireClient().secretStatus()) as SecretStatusPayload
  })

  ipcMain.handle(
    IPC.SecretPut,
    async (_e, value: string): Promise<{ ref: string; ok: boolean }> => {
      return (await requireClient().putSecret(value)) as { ref: string; ok: boolean }
    }
  )

  ipcMain.handle(IPC.ConfigGet, async (): Promise<RuntimeSettingsPayload> => {
    return (await requireClient().getSettings()) as RuntimeSettingsPayload
  })

  ipcMain.handle(
    IPC.ConfigSet,
    async (_e, payload: RuntimeSettingsPayload): Promise<{ ok: boolean }> => {
      return (await requireClient().applySettings(payload)) as { ok: boolean }
    }
  )

  ipcMain.handle(
    IPC.ModelList,
    async (_e, opts?: { base_url?: string }): Promise<ModelListPayload> => {
      return (await requireClient().listModels(opts)) as ModelListPayload
    }
  )

  // 无边框窗口的自绘控制。用 on 而非 handle：这些是单向命令，无需回值。
  ipcMain.on(IPC.WindowMinimize, () => {
    mainWindow?.minimize()
  })
  ipcMain.on(IPC.WindowMaximize, () => {
    if (!mainWindow) return
    if (mainWindow.isMaximized()) {
      mainWindow.unmaximize()
    } else {
      mainWindow.maximize()
    }
  })
  ipcMain.on(IPC.WindowClose, () => {
    mainWindow?.close()
  })
}

function createWindow(): void {
  mainWindow = new BrowserWindow({
    width: 1360,
    height: 900,
    minWidth: 960,
    minHeight: 640,
    show: false,
    // 无边框 + 自绘标题栏：玻璃质感的前提是窗口背景可以被 CSS 控制，
    // 系统原生标题栏会打断视觉连续性。
    frame: false,
    titleBarStyle: 'hidden',
    // 透明背景让 CSS 的 backdrop-filter 能取到真实的窗口后方内容，
    // 这是"玻璃"观感成立的关键（否则只能模糊到纯色，没有材质感）。
    backgroundColor: '#00000000',
    transparent: true,
    webPreferences: {
      preload: path.join(__dirname_, '../preload/index.mjs'),
      contextIsolation: true,
      nodeIntegration: false,
      sandbox: false
    }
  })

  mainWindow.on('ready-to-show', () => {
    mainWindow?.show()
  })

  // 外链一律用系统浏览器打开，不在应用内导航。
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    void shell.openExternal(url)
    return { action: 'deny' }
  })

  const devUrl = process.env['ELECTRON_RENDERER_URL']
  if (devUrl) {
    void mainWindow.loadURL(devUrl)
  } else {
    void mainWindow.loadFile(path.join(__dirname_, '../renderer/index.html'))
  }

  mainWindow.on('closed', () => {
    mainWindow = null
  })
}

app.whenReady().then(async () => {
  registerIpcHandlers()
  createWindow()

  // 启动后自动拉起后端；失败不阻塞 UI 显示，状态会推给界面由用户重试。
  ensureBackend()
    .then(broadcastStatus)
    .catch((err: Error) => broadcastStatus({ kind: 'error', message: err.message }))

  app.on('activate', () => {
    if (BrowserWindow.getAllWindows().length === 0) createWindow()
  })
})

app.on('window-all-closed', () => {
  if (process.platform !== 'darwin') {
    app.quit()
  }
})

// 退出前必须回收后端子进程，否则会留下孤儿 supervisor 占着端口。
app.on('before-quit', async (event) => {
  if (backend.getStatus().kind !== 'stopped') {
    event.preventDefault()
    client?.close()
    client = null
    await backend.stop()
    app.exit(0)
  }
})
