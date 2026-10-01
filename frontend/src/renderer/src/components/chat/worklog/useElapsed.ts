import { useEffect, useRef, useState } from 'react'

/**
 * useElapsed 返回从 `from` 到现在的已用毫秒数，运行中每 250ms 刷新一次。
 *
 * 两个细节是有意的：
 *   - 只有 `active` 为真时才起定时器。任务结束后停在最后一次读数，不再每 250ms
 *     唤醒一次渲染——一个已经结束的卡片不该持续消耗主线程。
 *   - 用 requestAnimationFrame 做节流而不是 setInterval：后台标签页里 rAF 会
 *     自然暂停，不会攒下一堆没人看的更新。
 */
export function useElapsed(from: number, active: boolean): number {
  const [now, setNow] = useState(() => Date.now())
  const rafRef = useRef<number | null>(null)

  useEffect(() => {
    if (!active) {
      setNow(Date.now())
      return
    }
    let cancelled = false
    let last = 0
    const tick = (t: number): void => {
      if (cancelled) return
      // 250ms 节流：读数变化足够可见，又不会每帧触发一次 React 渲染。
      if (t - last >= 250) {
        last = t
        setNow(Date.now())
      }
      rafRef.current = requestAnimationFrame(tick)
    }
    rafRef.current = requestAnimationFrame(tick)
    return () => {
      cancelled = true
      if (rafRef.current !== null) cancelAnimationFrame(rafRef.current)
      rafRef.current = null
    }
  }, [active])

  return Math.max(0, now - from)
}

/** fmtDuration 把毫秒渲染成人类可读的短时长。 */
export function fmtDuration(ms: number): string {
  if (ms < 1000) return `${Math.max(0, Math.round(ms))}ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m${s.toString().padStart(2, '0')}s`
}
