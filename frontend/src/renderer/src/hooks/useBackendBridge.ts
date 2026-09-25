import { useEffect } from 'react'
import { useStore } from '../store/app-store'
import { isTerminalState } from '@shared/types'

/**
 * 把主进程推来的事件与后端状态接到 store 上。
 *
 * 单独成 hook 的原因：订阅必须在应用生命周期内只建立一次，且要在组件卸载时
 * 正确退订（否则 StrictMode 的双挂载会导致事件被处理两遍）。
 */
export function useBackendBridge(): void {
  const setBackend = useStore((s) => s.setBackend)
  const applyEvent = useStore((s) => s.applyEvent)
  const setRunFromStatus = useStore((s) => s.setRunFromStatus)
  const runs = useStore((s) => s.runs)

  // 订阅后端状态与事件推送。
  useEffect(() => {
    const offStatus = window.ximo.onBackendStatus((st) => setBackend(st))
    const offEvent = window.ximo.onEvent((ev) => applyEvent(ev))

    // 主动问一次当前状态，避免订阅之前发生的变化被漏掉。
    void window.ximo.backendStatus().then(setBackend)

    return () => {
      offStatus()
      offEvent()
    }
  }, [setBackend, applyEvent])

  /**
   * 兜底对账：事件流可能因重连丢帧，而界面不能永远停在"执行中"。
   *
   * 每 3 秒对非终态的 run 查一次状态接口；若后端已判定终态而界面还没收到终态
   * 事件，就以状态接口为准补齐。这是"事件流为主、轮询兜底"的双保险：
   * 正常路径完全靠事件（实时、精确），异常路径不会卡死。
   */
  useEffect(() => {
    const timer = setInterval(() => {
      const current = useStore.getState().runs
      for (const view of Object.values(current)) {
        if (isTerminalState(view.state)) continue
        // 临时 pending run 没有真实 runID，跳过。
        if (view.runId.startsWith('pending-')) continue
        void window.ximo
          .status(view.runId)
          .then(setRunFromStatus)
          .catch(() => {
            // 查询失败（例如后端重启中）不算错误：下一轮再试。
          })
      }
    }, 3000)
    return () => clearInterval(timer)
    // runs 只用于决定是否启动定时器，不参与依赖，避免每次事件都重建定时器。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [setRunFromStatus, Object.keys(runs).length])

  // 断线重连后补拉缺失事件（第26章）。
  useEffect(() => {
    const off = window.ximo.onBackendStatus((st) => {
      if (st.kind !== 'ready') return
      const current = useStore.getState().runs
      for (const view of Object.values(current)) {
        if (view.runId.startsWith('pending-')) continue
        void window.ximo
          .events(view.runId, view.lastSeq)
          .then((payload) => {
            for (const ev of payload.events) {
              applyEvent(ev)
            }
          })
          .catch(() => {
            // 同上：重连期间拉取失败可忽略，下一轮对账会补上。
          })
      }
    })
    return off
  }, [applyEvent])

  /**
   * 活动事件排空：run 未终态期间持续拉取增量事件。
   *
   * 这是消息内容到达界面的主通道：token.delta 属于临时事件（不落持久化日志，
   * 且只有存在订阅者时才会被投递），final_answer 等持久化事件虽然落库，但也
   * 必须有人来拉才会出现在界面上。后端的事件接口是"空闲即返回"的有界长轮询，
   * 这里对每个活动 run 反复拉取直到终态，流式输出即以秒级延迟呈现。
   */
  useEffect(() => {
    const draining = new Set<string>()

    const drainOnce = async (runId: string): Promise<void> => {
      // 每轮循环内重复拉取，直到这一轮没有新事件（More=false）。
      for (;;) {
        const view = useStore.getState().runs[runId]
        if (!view || isTerminalState(view.state)) return
        try {
          const payload = await window.ximo.events(runId, view.lastSeq)
          for (const ev of payload.events) {
            applyEvent(ev)
          }
          if (!payload.more && payload.events.length === 0) return
        } catch {
          // 拉取失败（后端重启/请求超时）：交给下一轮 tick 重试。
          return
        }
      }
    }

    const timer = setInterval(() => {
      const current = useStore.getState().runs
      for (const view of Object.values(current)) {
        if (isTerminalState(view.state)) continue
        // 临时 pending run 没有真实 runID，跳过。
        if (view.runId.startsWith('pending-')) continue
        if (draining.has(view.runId)) continue
        draining.add(view.runId)
        void drainOnce(view.runId).finally(() => draining.delete(view.runId))
      }
    }, 1000)
    return () => clearInterval(timer)
  }, [applyEvent])
}
