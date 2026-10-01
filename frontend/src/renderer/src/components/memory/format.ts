/**
 * 记忆页的展示辅助函数（纯函数，无 React）。
 *
 * 单独成文件的理由：时间格式化与权重条的百分比是「看起来不对但不会报错」的地方，
 * 抽出来就能直接断言边界（0 值、未来时间、负权重）。
 */

import type { MemoryGraphEdge } from '@shared/types'

/** 把毫秒时间戳格式化成 YYYY-MM-DD HH:mm；0/非法值返回占位符。 */
export function fmtTime(ms: number | undefined, placeholder = '—'): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return placeholder
  const d = new Date(ms)
  if (Number.isNaN(d.getTime())) return placeholder
  const p = (n: number): string => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/** 相对时间：3 分钟前 / 2 天前。 */
export function fmtRelative(ms: number | undefined, now = Date.now()): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return '从未'
  const diff = now - ms
  if (diff < 0) return '刚刚'
  const min = Math.floor(diff / 60_000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const hour = Math.floor(min / 60)
  if (hour < 24) return `${hour} 小时前`
  const day = Math.floor(hour / 24)
  if (day < 30) return `${day} 天前`
  const month = Math.floor(day / 30)
  if (month < 12) return `${month} 个月前`
  return `${Math.floor(month / 12)} 年前`
}

/** 权重条的宽度百分比（0-100），对 NaN/越界值做夹取。 */
export function weightPercent(w: number): number {
  if (!Number.isFinite(w)) return 0
  return Math.round(Math.max(0, Math.min(1, w)) * 100)
}

/** 边的有效权重（缺失时退回原始权重）。 */
export function effWeight(e: Pick<MemoryGraphEdge, 'effective_weight' | 'weight'>): number {
  return Number.isFinite(e.effective_weight) ? e.effective_weight : e.weight
}

/** 一行摘要：把正文压成单行，最多 n 个字符。 */
export function oneLine(text: string | undefined, n = 60): string {
  if (!text) return ''
  const s = text.replace(/\s+/g, ' ').trim()
  return s.length > n ? s.slice(0, n) + '…' : s
}
