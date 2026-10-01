import { describe, expect, it } from 'vitest'
import {
  DEFAULT_VIEWPORT,
  MAX_ZOOM,
  MIN_ZOOM,
  edgeKey,
  fitViewport,
  hitTest,
  panBy,
  screenToWorld,
  toLayoutNodes,
  worldToScreen,
  zoomAt,
  type LayoutNode
} from './graph-layout'
import type { MemoryGraphNode } from '@shared/types'

/**
 * 图布局的纯函数单测。
 *
 * 这些函数是「差一像素就点不中」的地方：命中判定、缩放锚点、fit 的边界。
 * 全部不依赖 DOM，因此可以直接断言数值。
 */

function gnode(id: string, extra: Partial<MemoryGraphNode> = {}): MemoryGraphNode {
  return {
    id,
    kind: 'fact',
    importance: 0.5,
    pinned: false,
    status: 'active',
    use_count: 0,
    degree: 0,
    created_at: 0,
    ...extra
  }
}

function lnode(id: string, x: number, y: number, r = 6): LayoutNode {
  return { ...gnode(id), x, y, r }
}

describe('坐标换算', () => {
  it('worldToScreen 与 screenToWorld 互为逆运算', () => {
    const vp = { k: 1.7, tx: -30, ty: 44 }
    const s = worldToScreen(vp, 12, -8)
    const w = screenToWorld(vp, s.x, s.y)
    expect(w.x).toBeCloseTo(12)
    expect(w.y).toBeCloseTo(-8)
  })
})

describe('缩放', () => {
  it('zoomAt 保持锚点下的 world 坐标不动', () => {
    const vp = { k: 1, tx: 0, ty: 0 }
    const before = screenToWorld(vp, 100, 50)
    const next = zoomAt(vp, 100, 50, 2)
    const after = screenToWorld(next, 100, 50)
    expect(next.k).toBe(2)
    expect(after.x).toBeCloseTo(before.x)
    expect(after.y).toBeCloseTo(before.y)
  })

  it('缩放被夹在 [MIN_ZOOM, MAX_ZOOM] 内', () => {
    let vp = { k: 1, tx: 0, ty: 0 }
    for (let i = 0; i < 40; i++) vp = zoomAt(vp, 0, 0, 2)
    expect(vp.k).toBe(MAX_ZOOM)
    for (let i = 0; i < 80; i++) vp = zoomAt(vp, 0, 0, 0.5)
    expect(vp.k).toBe(MIN_ZOOM)
  })
})

describe('平移与 fit', () => {
  it('panBy 只改平移量', () => {
    const vp = panBy({ k: 1.2, tx: 5, ty: 6 }, -3, 4)
    expect(vp).toEqual({ k: 1.2, tx: 2, ty: 10 })
  })

  it('fitViewport 让所有节点落在画布内', () => {
    const nodes = [lnode('a', -100, -60, 10), lnode('b', 120, 80, 10), lnode('c', 0, 0, 10)]
    const size = { width: 800, height: 600 }
    const vp = fitViewport(nodes, size, 40)
    for (const n of nodes) {
      const s = worldToScreen(vp, n.x, n.y)
      expect(s.x).toBeGreaterThanOrEqual(0)
      expect(s.x).toBeLessThanOrEqual(size.width)
      expect(s.y).toBeGreaterThanOrEqual(0)
      expect(s.y).toBeLessThanOrEqual(size.height)
    }
  })

  it('空图或零尺寸返回默认视口（不产生 NaN）', () => {
    expect(fitViewport([], { width: 800, height: 600 })).toEqual(DEFAULT_VIEWPORT)
    const vp = fitViewport([lnode('a', 1, 1)], { width: 0, height: 0 })
    expect(Number.isFinite(vp.k)).toBe(true)
    expect(vp).toEqual(DEFAULT_VIEWPORT)
  })
})

describe('命中判定', () => {
  it('点中节点返回该节点，点空处返回 null', () => {
    const nodes = [lnode('a', 0, 0, 10), lnode('b', 100, 0, 10)]
    const vp = { k: 1, tx: 200, ty: 100 }
    const aScreen = worldToScreen(vp, 0, 0)
    expect(hitTest(nodes, vp, aScreen.x, aScreen.y)?.id).toBe('a')
    expect(hitTest(nodes, vp, 500, 500)).toBeNull()
  })

  it('重叠时返回后画的（数组末尾）节点', () => {
    const nodes = [lnode('under', 0, 0, 12), lnode('over', 2, 0, 12)]
    expect(hitTest(nodes, DEFAULT_VIEWPORT, 2, 0)?.id).toBe('over')
  })

  it('半径随缩放放大，命中范围跟着放大', () => {
    const nodes = [lnode('a', 0, 0, 5)]
    // k=4 时半径 20px：距离 18px 处应当命中。
    expect(hitTest(nodes, { k: 4, tx: 0, ty: 0 }, 18, 0)?.id).toBe('a')
    // k=1 时半径 5px：同样距离点不中。
    expect(hitTest(nodes, { k: 1, tx: 0, ty: 0 }, 18, 0)).toBeNull()
  })
})

describe('布局节点构建', () => {
  it('沿用上一次的位置，避免重排时整张图跳动', () => {
    const prev = new Map<string, LayoutNode>([['a', lnode('a', 33, 44)]])
    const out = toLayoutNodes([gnode('a'), gnode('b')], prev)
    expect(out[0].x).toBe(33)
    expect(out[0].y).toBe(44)
    // 新节点没有历史位置：给 NaN，交给调用方做圆周初始化。
    expect(Number.isNaN(out[1].x)).toBe(true)
  })

  it('半径按 importance 与 degree 计算', () => {
    const out = toLayoutNodes([gnode('a', { importance: 1, degree: 9 })], new Map())
    expect(out[0].r).toBeGreaterThan(8)
  })
})

describe('边键', () => {
  it('edgeKey 有向且稳定（同向同键，反向不同键）', () => {
    expect(edgeKey({ src: 'a', dst: 'b' })).toBe(edgeKey({ src: 'a', dst: 'b' }))
    expect(edgeKey({ src: 'a', dst: 'b' })).not.toBe(edgeKey({ src: 'b', dst: 'a' }))
  })
})
