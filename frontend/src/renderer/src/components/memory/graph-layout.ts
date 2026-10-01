/**
 * 力导向图的数据结构与视口变换（纯逻辑，无 DOM）。
 *
 * 拆出来的理由：拖拽命中判定、缩放换算、fit-to-view 这些是「差一个像素就点不中」
 * 的地方，必须能直接单测；把它们塞进 Canvas 组件就只能靠肉眼看画面。
 *
 * 坐标系约定（两个坐标系，绝不混用）：
 *   world  —— 仿真坐标，节点位置由 d3-force 写在这里，与画布大小无关；
 *   screen —— 画布像素坐标，等于 world * k + tx/ty。
 */

import type { MemoryGraphEdge, MemoryGraphNode } from '@shared/types'
import { nodeRadius } from '../../store/memory-store'

/** 视口：world → screen 的平移缩放。 */
export interface Viewport {
  /** 缩放系数（>1 放大）。 */
  k: number
  /** 平移量（屏幕像素）。 */
  tx: number
  ty: number
}

export const DEFAULT_VIEWPORT: Viewport = { k: 1, tx: 0, ty: 0 }

/** 缩放的上下限。下限保证图不会缩成一个点，上限保证不会放大到看不见结构。 */
export const MIN_ZOOM = 0.15
export const MAX_ZOOM = 6

/** 参与仿真的节点：在图节点上挂 d3 需要的可变字段。 */
export interface LayoutNode extends MemoryGraphNode {
  x: number
  y: number
  vx?: number
  vy?: number
  fx?: number | null
  fy?: number | null
  index?: number
  /** 半径（缓存，避免每帧重算）。 */
  r: number
}

/** 参与仿真的边：d3-force 会把 source/target 解析成节点对象引用。 */
export interface LayoutLink {
  source: string | LayoutNode
  target: string | LayoutNode
  edge: MemoryGraphEdge
  index?: number
}

/** 画布尺寸（CSS 像素，不含 devicePixelRatio）。 */
export interface Size {
  width: number
  height: number
}

export function worldToScreen(vp: Viewport, x: number, y: number): { x: number; y: number } {
  return { x: x * vp.k + vp.tx, y: y * vp.k + vp.ty }
}

export function screenToWorld(vp: Viewport, x: number, y: number): { x: number; y: number } {
  return { x: (x - vp.tx) / vp.k, y: (y - vp.ty) / vp.k }
}

/** 以屏幕上某个锚点为中心缩放（滚轮缩放的标准语义）。 */
export function zoomAt(vp: Viewport, screenX: number, screenY: number, factor: number): Viewport {
  const k = clamp(vp.k * factor, MIN_ZOOM, MAX_ZOOM)
  if (k === vp.k) return vp
  // 保持锚点下的 world 坐标不动：先算锚点的 world 坐标，再用新 k 反解平移。
  const w = screenToWorld(vp, screenX, screenY)
  return { k, tx: screenX - w.x * k, ty: screenY - w.y * k }
}

/** 平移视口。 */
export function panBy(vp: Viewport, dx: number, dy: number): Viewport {
  return { k: vp.k, tx: vp.tx + dx, ty: vp.ty + dy }
}

/**
 * 把整张图装进画布（fit-to-view），留出 padding 边距。
 *
 * 空图或尺寸为 0 时返回默认视口：除零会得到 NaN，而 NaN 一旦进了 transform，
 * 画布就永远空白，且没有任何报错。
 */
export function fitViewport(
  nodes: { x: number; y: number; r: number }[],
  size: Size,
  padding = 40
): Viewport {
  if (nodes.length === 0 || size.width <= 0 || size.height <= 0) return { ...DEFAULT_VIEWPORT }
  let minX = Infinity
  let minY = Infinity
  let maxX = -Infinity
  let maxY = -Infinity
  for (const n of nodes) {
    if (!Number.isFinite(n.x) || !Number.isFinite(n.y)) continue
    minX = Math.min(minX, n.x - n.r)
    minY = Math.min(minY, n.y - n.r)
    maxX = Math.max(maxX, n.x + n.r)
    maxY = Math.max(maxY, n.y + n.r)
  }
  if (!Number.isFinite(minX) || !Number.isFinite(minY)) return { ...DEFAULT_VIEWPORT }
  const w = Math.max(1, maxX - minX)
  const h = Math.max(1, maxY - minY)
  const k = clamp(
    Math.min((size.width - padding * 2) / w, (size.height - padding * 2) / h),
    MIN_ZOOM,
    MAX_ZOOM
  )
  const cx = (minX + maxX) / 2
  const cy = (minY + maxY) / 2
  return { k, tx: size.width / 2 - cx * k, ty: size.height / 2 - cy * k }
}

/**
 * 命中判定：返回最上层（后画的）被点中的节点。
 *
 * 从数组尾部往前找，与绘制顺序一致——否则点两个重叠节点时选中的是下面那个，
 * 用户会觉得"点错了"。
 */
export function hitTest(
  nodes: LayoutNode[],
  vp: Viewport,
  screenX: number,
  screenY: number,
  slack = 2
): LayoutNode | null {
  for (let i = nodes.length - 1; i >= 0; i--) {
    const n = nodes[i]
    const s = worldToScreen(vp, n.x, n.y)
    const rr = n.r * vp.k + slack
    const dx = screenX - s.x
    const dy = screenY - s.y
    if (dx * dx + dy * dy <= rr * rr) return n
  }
  return null
}

/** 给一批图节点建仿真节点：已有位置的沿用（避免重排时整张图跳动）。 */
export function toLayoutNodes(
  nodes: MemoryGraphNode[],
  prev: Map<string, LayoutNode>
): LayoutNode[] {
  return nodes.map((n) => {
    const old = prev.get(n.id)
    return {
      ...n,
      x: old?.x ?? Number.NaN,
      y: old?.y ?? Number.NaN,
      vx: old?.vx,
      vy: old?.vy,
      r: nodeRadius(n)
    }
  })
}

/** 边的稳定键，供点亮集合与 React key 使用。 */
export function edgeKey(e: Pick<MemoryGraphEdge, 'src' | 'dst'>): string {
  return `${e.src}\u0000${e.dst}`
}

function clamp(v: number, lo: number, hi: number): number {
  if (!Number.isFinite(v)) return lo
  return v < lo ? lo : v > hi ? hi : v
}
