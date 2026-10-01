import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  forceCenter,
  forceCollide,
  forceLink,
  forceManyBody,
  forceSimulation,
  type Simulation
} from 'd3-force'
import type { MemoryGraphEdge, MemoryGraphNode } from '@shared/types'
import { edgeWidth, kindColorVar, MEMORY_KIND_LABELS } from '../../store/memory-store'
import {
  DEFAULT_VIEWPORT,
  edgeKey,
  fitViewport,
  hitTest,
  panBy,
  screenToWorld,
  toLayoutNodes,
  worldToScreen,
  zoomAt,
  type LayoutLink,
  type LayoutNode,
  type Size,
  type Viewport
} from './graph-layout'

/**
 * 记忆网络视图（力导向图）。
 *
 * 为什么用 Canvas 而不是 SVG：审核文档 4.9 要求「>500 节点时不用 SVG」。记忆库
 * 的容量上限是 5 万节点，SVG 的 DOM 节点数会让主线程卡死；Canvas 每帧只画一次，
 * 节点数与帧耗时的关系是线性的。
 *
 * 为什么自己管 requestAnimationFrame 而不是靠 d3 的 tick 事件重绘：拖拽、缩放、
 * 高亮这些交互不推进仿真，但必须重绘。统一成「有变化就置脏 + 单循环渲染」比
 * 分散在几个事件处理里重绘更不容易漏（漏了的表现是画面卡住不动）。
 *
 * 力参数是刻意的：charge 用 -140 而不是 d3 默认的 -30，因为记忆图的节点普遍
 * 有 5-20 条边，弱斥力会让整张图缩成一个球；collide 用节点半径保证圆圈不重叠。
 */
interface CanvasGraphProps {
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  /** 当前选中节点 id。 */
  selectedId: string | null
  /** 搜索命中的节点 id（其余节点变暗）。 */
  searchHits: string[]
  /** 正在脉冲发光的节点 id。 */
  litNodes: string[]
  /** 正在流光的边键（`src\0dst`）。 */
  litEdges: string[]
  /** 点亮排期（含 delayMs，用于「依次」）。 */
  pulses: { id: string; delayMs: number }[]
  /** 是否尊重 prefers-reduced-motion。 */
  reducedMotion: boolean
  onSelect: (id: string) => void
}

/** 主题色解析结果：把 CSS 变量（"99 102 241"）读成 rgb 三元组字符串。 */
type Palette = Record<string, string>

function readPalette(el: HTMLElement): Palette {
  const cs = getComputedStyle(el)
  const keys = [
    '--c-accent',
    '--c-success',
    '--c-warning',
    '--c-danger',
    '--c-ink',
    '--c-ink-muted',
    '--c-ink-faint',
    '--c-border'
  ]
  const out: Palette = {}
  for (const k of keys) {
    const v = cs.getPropertyValue(k).trim()
    out[k] = v === '' ? '148 148 148' : v
  }
  return out
}

/** 把 "99 102 241" + alpha 组成 rgba()。 */
function rgba(triplet: string, alpha: number): string {
  return `rgba(${triplet.split(/\s+/).join(',')}, ${alpha})`
}

export function CanvasGraph({
  nodes,
  edges,
  selectedId,
  searchHits,
  litNodes,
  litEdges,
  pulses,
  reducedMotion,
  onSelect
}: CanvasGraphProps): React.JSX.Element {
  const wrapRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement>(null)

  const [size, setSize] = useState<Size>({ width: 0, height: 0 })
  const [hoverId, setHoverId] = useState<string | null>(null)

  // 仿真与布局对象放在 ref 里：它们每帧都在变，进 state 会触发无意义的重渲染。
  const simRef = useRef<Simulation<LayoutNode> | null>(null)
  const nodesRef = useRef<LayoutNode[]>([])
  const linksRef = useRef<LayoutLink[]>([])
  const prevPosRef = useRef(new Map<string, LayoutNode>())
  const vpRef = useRef<Viewport>({ ...DEFAULT_VIEWPORT })
  const sizeRef = useRef<Size>(size)
  const paletteRef = useRef<Palette>({})
  const dirtyRef = useRef(true)
  const frameRef = useRef(0)
  const animRef = useRef(0)
  /** 交互状态：拖拽节点 / 平移画布。 */
  const dragRef = useRef<{ node: LayoutNode | null; moved: boolean; sx: number; sy: number } | null>(
    null
  )
  const panRef = useRef<{ sx: number; sy: number } | null>(null)
  /** 最近一次点亮的时间戳，用于计算脉冲相位。 */
  const litStartRef = useRef(0)
  /** 渲染函数的间接引用（见下面「渲染循环」的注释）。 */
  const drawRef = useRef<() => void>(() => {})

  // 传给渲染循环的最新 props：用 ref 避免每帧闭包捕获旧值，也避免重建循环。
  const propsRef = useRef({
    selectedId,
    searchHits,
    litNodes,
    litEdges,
    pulses,
    reducedMotion,
    hoverId
  })
  propsRef.current = { selectedId, searchHits, litNodes, litEdges, pulses, reducedMotion, hoverId }

  // -------------------------------------------------------------------------
  // 尺寸
  // -------------------------------------------------------------------------
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    const apply = (): void => {
      const rect = el.getBoundingClientRect()
      const next = { width: Math.max(1, Math.floor(rect.width)), height: Math.max(1, Math.floor(rect.height)) }
      sizeRef.current = next
      setSize(next)
      dirtyRef.current = true
    }
    apply()
    if (typeof ResizeObserver === 'undefined') {
      window.addEventListener('resize', apply)
      return () => window.removeEventListener('resize', apply)
    }
    const ro = new ResizeObserver(apply)
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  // -------------------------------------------------------------------------
  // 仿真：节点/边变化时重建（沿用旧位置，避免整张图跳动）
  // -------------------------------------------------------------------------
  useEffect(() => {
    const layoutNodes = toLayoutNodes(nodes, prevPosRef.current)
    const ids = new Set(layoutNodes.map((n) => n.id))
    const layoutLinks: LayoutLink[] = edges
      .filter((e) => ids.has(e.src) && ids.has(e.dst))
      .map((e) => ({ source: e.src, target: e.dst, edge: e }))

    // 新节点给一个圆周初值：全放在原点会让斥力在第一步产生巨大数值，图会炸开。
    const n = layoutNodes.length
    layoutNodes.forEach((node, i) => {
      if (!Number.isFinite(node.x) || !Number.isFinite(node.y)) {
        const a = (i / Math.max(1, n)) * Math.PI * 2
        const radius = 40 + (i % 12) * 14
        node.x = Math.cos(a) * radius
        node.y = Math.sin(a) * radius
      }
    })

    nodesRef.current = layoutNodes
    linksRef.current = layoutLinks

    simRef.current?.stop()
    const sim = forceSimulation<LayoutNode>(layoutNodes)
      .force(
        'link',
        forceLink<LayoutNode, LayoutLink>(layoutLinks)
          .id((d) => d.id)
          // 边越强越短：权重高的记忆关联在视觉上就该更紧。
          .distance((l) => 90 - 45 * clamp01(l.edge.effective_weight))
          .strength((l) => 0.08 + 0.42 * clamp01(l.edge.effective_weight))
          .iterations(1)
      )
      .force('charge', forceManyBody<LayoutNode>().strength(-140).distanceMax(600))
      .force('center', forceCenter<LayoutNode>(0, 0).strength(0.05))
      .force('collide', forceCollide<LayoutNode>((d) => d.r + 4).iterations(1))
      .alpha(0.9)
      .alphaDecay(0.028)
      .velocityDecay(0.35)

    sim.on('tick', () => {
      dirtyRef.current = true
    })
    simRef.current = sim

    // 首次装载后自动 fit：用户打开记忆页应该直接看到全图，而不是一堆重叠的圆。
    if (prevPosRef.current.size === 0 && layoutNodes.length > 0) {
      sim.tick(60)
      vpRef.current = fitViewport(layoutNodes, sizeRef.current)
    }
    prevPosRef.current = new Map(layoutNodes.map((nd) => [nd.id, nd]))
    dirtyRef.current = true

    return () => {
      sim.stop()
    }
    // edges 参与依赖是必需的：边的变化同样要重建 link 力。
  }, [nodes, edges])

  // -------------------------------------------------------------------------
  // 主题色：主题切换时重新读一遍 CSS 变量
  // -------------------------------------------------------------------------
  useEffect(() => {
    const el = wrapRef.current
    if (!el) return
    const refresh = (): void => {
      paletteRef.current = readPalette(el)
      dirtyRef.current = true
    }
    refresh()
    // 主题切换是往 :root 上写 CSS 变量，没有事件；用 MutationObserver 监听 style 属性。
    const target = document.documentElement
    if (typeof MutationObserver === 'undefined') return
    const mo = new MutationObserver(refresh)
    mo.observe(target, { attributes: true, attributeFilter: ['style', 'class', 'data-theme'] })
    return () => mo.disconnect()
  }, [])

  // -------------------------------------------------------------------------
  // 点亮动画：记录起点时间，让渲染层算出相位
  // -------------------------------------------------------------------------
  useEffect(() => {
    if (litNodes.length === 0) {
      animRef.current = 0
      dirtyRef.current = true
      return
    }
    litStartRef.current = performance.now()
    animRef.current = 1
    dirtyRef.current = true
  }, [litNodes, litEdges, pulses])

  // -------------------------------------------------------------------------
  // 渲染循环
  //
  // 用一个 ref 间接引用 draw：draw 在下面才定义（它需要用到本组件全部 ref），
  // 而渲染循环必须在挂载时只启动一次。循环体本身只读 ref 与 propsRef，所以
  // 不需要重建，也不会读到旧闭包。
  // -------------------------------------------------------------------------
  useEffect(() => {
    const loop = (): void => {
      frameRef.current = requestAnimationFrame(loop)
      const p = propsRef.current
      const animating = animRef.current === 1 && !p.reducedMotion
      if (!dirtyRef.current && !animating) return
      dirtyRef.current = false
      drawRef.current()
    }
    frameRef.current = requestAnimationFrame(loop)
    return () => cancelAnimationFrame(frameRef.current)
  }, [])

  const draw = useCallback((): void => {
    const canvas = canvasRef.current
    if (!canvas) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return

    const { width, height } = sizeRef.current
    const dpr = typeof window === 'undefined' ? 1 : window.devicePixelRatio || 1
    const pw = Math.max(1, Math.floor(width * dpr))
    const ph = Math.max(1, Math.floor(height * dpr))
    if (canvas.width !== pw || canvas.height !== ph) {
      canvas.width = pw
      canvas.height = ph
    }
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    ctx.clearRect(0, 0, width, height)

    const vp = vpRef.current
    const pal = paletteRef.current
    const p = propsRef.current
    const nodesL = nodesRef.current
    const linksL = linksRef.current
    const lit = new Set(p.litNodes)
    const litE = new Set(p.litEdges)
    const hits = new Set(p.searchHits)
    const searching = hits.size > 0
    const now = performance.now()

    // ---- 边 ----
    for (const l of linksL) {
      const s = l.source
      const t = l.target
      if (typeof s === 'string' || typeof t === 'string') continue
      const a = worldToScreen(vp, s.x, s.y)
      const b = worldToScreen(vp, t.x, t.y)
      const key = edgeKey(l.edge)
      const isLit = litE.has(key)
      // 搜索/选中态下，无关的边要淡下去，否则高亮被噪声淹没。
      const focusId = p.selectedId
      const related =
        !focusId || l.edge.src === focusId || l.edge.dst === focusId || lit.has(l.edge.src) || lit.has(l.edge.dst)
      const alpha = (searching && !related ? 0.08 : related ? 0.5 : 0.16) + (isLit ? 0.3 : 0)
      // 粗细只按 effective_weight：原始权重会让早已失效的连接看起来仍然很粗。
      ctx.strokeStyle = rgba(pal['--c-border'] ?? '148 148 148', Math.min(0.95, alpha))
      ctx.lineWidth = edgeWidth(l.edge) * Math.max(0.5, Math.min(1.6, vp.k))
      ctx.beginPath()
      ctx.moveTo(a.x, a.y)
      ctx.lineTo(b.x, b.y)
      ctx.stroke()

      if (isLit) {
        // 流光：一条虚线沿边滑动（复用 Work Log 的「流动」语义）。
        const phase = reducedMotion ? 0 : (now / 24) % 16
        ctx.save()
        ctx.setLineDash([5, 11])
        ctx.lineDashOffset = -phase
        ctx.strokeStyle = rgba(pal['--c-accent'] ?? '99 102 241', 0.95)
        ctx.lineWidth = Math.max(1.6, edgeWidth(l.edge) * 1.3)
        ctx.beginPath()
        ctx.moveTo(a.x, a.y)
        ctx.lineTo(b.x, b.y)
        ctx.stroke()
        ctx.restore()
      }
    }

    // ---- 节点 ----
    for (const n of nodesL) {
      const s = worldToScreen(vp, n.x, n.y)
      // 视口外的节点跳过绘制：5 万节点的库里这一步省掉绝大部分工作。
      if (s.x < -40 || s.y < -40 || s.x > width + 40 || s.y > height + 40) continue
      const r = Math.max(1.5, n.r * vp.k)
      const isLit = lit.has(n.id)
      const isSel = p.selectedId === n.id
      const isHover = p.hoverId === n.id
      const isHit = hits.has(n.id)
      const dim = (searching && !isHit && !isSel) || (p.selectedId !== null && !isSel && !isLit && !isHover)
      const color = pal[kindColorVar(n.kind)] ?? pal['--c-ink-muted'] ?? '148 148 148'

      // 点亮脉冲：按 pulses 里的 delayMs 依次起亮，每个亮 RECALL_PULSE_MS 后回落。
      let glow = 0
      if (isLit && !reducedMotion) {
        const delay = p.pulses.find((x) => x.id === n.id)?.delayMs ?? 0
        const t = now - litStartRef.current - delay
        if (t > 0) {
          const phase = Math.min(1, t / 1400)
          glow = Math.sin(Math.PI * phase) // 0→1→0
        }
      }

      if (glow > 0) {
        ctx.beginPath()
        ctx.arc(s.x, s.y, r + 10 * glow + 4, 0, Math.PI * 2)
        ctx.fillStyle = rgba(pal['--c-accent'] ?? '99 102 241', 0.22 * glow)
        ctx.fill()
      }

      ctx.beginPath()
      ctx.arc(s.x, s.y, r, 0, Math.PI * 2)
      ctx.fillStyle = rgba(color, dim ? 0.32 : 0.92)
      ctx.fill()

      // 置顶节点画一圈金环；选中/命中画强调色描边。
      if (n.pinned) {
        ctx.beginPath()
        ctx.arc(s.x, s.y, r + 2.5, 0, Math.PI * 2)
        ctx.strokeStyle = rgba(pal['--c-warning'] ?? '245 158 11', dim ? 0.3 : 0.95)
        ctx.lineWidth = 1.4
        ctx.stroke()
      }
      if (isSel || isHit || isHover) {
        ctx.beginPath()
        ctx.arc(s.x, s.y, r + 1.8, 0, Math.PI * 2)
        ctx.strokeStyle = rgba(pal['--c-accent'] ?? '99 102 241', isSel ? 1 : 0.8)
        ctx.lineWidth = isSel ? 2 : 1.3
        ctx.stroke()
      }

      // 标签：放大到一定程度、或该节点正在发光/被选中时才画，避免密集时糊成一片。
      const showLabel = (vp.k >= 1.3 || isSel || isHover || isLit) && !dim
      if (showLabel && n.title) {
        ctx.font = `${isSel ? '600 ' : ''}11px var(--font-ui, sans-serif)`
        ctx.fillStyle = rgba(pal['--c-ink'] ?? '245 245 245', isSel || isLit ? 0.95 : 0.72)
        ctx.textAlign = 'center'
        ctx.textBaseline = 'top'
        const label = n.title.length > 14 ? n.title.slice(0, 14) + '…' : n.title
        ctx.fillText(label, s.x, s.y + r + 3)
      }
    }
    // 只读 ref（nodesRef/linksRef/vpRef/paletteRef/propsRef），不需要任何依赖。
  }, [])

  // 把最新的 draw 交给渲染循环（每次渲染都重新指向，避免闭包过期）。
  drawRef.current = draw

  // -------------------------------------------------------------------------
  // 交互
  // -------------------------------------------------------------------------
  const localPoint = (e: React.PointerEvent | React.MouseEvent | React.WheelEvent): { x: number; y: number } => {
    const rect = (e.currentTarget as HTMLElement).getBoundingClientRect()
    return { x: e.clientX - rect.left, y: e.clientY - rect.top }
  }

  const handlePointerDown = (e: React.PointerEvent<HTMLCanvasElement>): void => {
    const pt = localPoint(e)
    const hit = hitTest(nodesRef.current, vpRef.current, pt.x, pt.y)
    // 中键或按住空格平移；命中节点则拖拽节点；否则平移画布。
    if (e.button === 1 || e.shiftKey) {
      panRef.current = { sx: pt.x, sy: pt.y }
    } else if (hit) {
      dragRef.current = { node: hit, moved: false, sx: pt.x, sy: pt.y }
      const sim = simRef.current
      if (sim) {
        hit.fx = hit.x
        hit.fy = hit.y
        sim.alphaTarget(0.2).restart()
      }
    } else {
      panRef.current = { sx: pt.x, sy: pt.y }
    }
    e.currentTarget.setPointerCapture(e.pointerId)
  }

  const handlePointerMove = (e: React.PointerEvent<HTMLCanvasElement>): void => {
    const pt = localPoint(e)
    const drag = dragRef.current
    if (drag?.node) {
      const w = screenToWorld(vpRef.current, pt.x, pt.y)
      drag.node.fx = w.x
      drag.node.fy = w.y
      if (Math.abs(pt.x - drag.sx) + Math.abs(pt.y - drag.sy) > 3) drag.moved = true
      dirtyRef.current = true
      return
    }
    const pan = panRef.current
    if (pan) {
      vpRef.current = panBy(vpRef.current, pt.x - pan.sx, pt.y - pan.sy)
      pan.sx = pt.x
      pan.sy = pt.y
      dirtyRef.current = true
      return
    }
    // 悬停高亮：只在命中项变化时 setState，避免每次移动都触发重渲染。
    const hit = hitTest(nodesRef.current, vpRef.current, pt.x, pt.y)
    const id = hit?.id ?? null
    setHoverId((prev) => (prev === id ? prev : id))
  }

  const endPointer = (e: React.PointerEvent<HTMLCanvasElement>): void => {
    const drag = dragRef.current
    if (drag?.node) {
      // 松手后解钉，但保留当前位置：节点应停在用户放下的地方附近继续被力平衡。
      drag.node.fx = null
      drag.node.fy = null
      simRef.current?.alphaTarget(0)
      if (!drag.moved) onSelect(drag.node.id)
    }
    dragRef.current = null
    panRef.current = null
    if (e.currentTarget.hasPointerCapture(e.pointerId)) {
      e.currentTarget.releasePointerCapture(e.pointerId)
    }
    dirtyRef.current = true
  }

  const handleWheel = (e: React.WheelEvent<HTMLCanvasElement>): void => {
    const pt = localPoint(e)
    const factor = Math.pow(0.999, e.deltaY)
    vpRef.current = zoomAt(vpRef.current, pt.x, pt.y, factor)
    dirtyRef.current = true
  }

  const fit = useCallback((): void => {
    vpRef.current = fitViewport(nodesRef.current, sizeRef.current)
    dirtyRef.current = true
  }, [])

  const pulseNow = useMemo(() => pulses.length > 0, [pulses])

  return (
    <div ref={wrapRef} className="relative h-full w-full overflow-hidden">
      <canvas
        ref={canvasRef}
        className="block h-full w-full touch-none"
        style={{
          width: size.width || '100%',
          height: size.height || '100%',
          cursor: hoverId ? 'pointer' : 'grab'
        }}
        onPointerDown={handlePointerDown}
        onPointerMove={handlePointerMove}
        onPointerUp={endPointer}
        onPointerCancel={endPointer}
        onWheel={handleWheel}
        onDoubleClick={fit}
        aria-label={`记忆网络图，共 ${nodes.length} 个节点、${edges.length} 条连接`}
        role="img"
      />

      {/* 浮动控制条：重置视口 + 图例。 */}
      <div className="pointer-events-none absolute bottom-3 left-3 flex flex-col gap-2">
        <div className="glass-panel pointer-events-auto flex items-center gap-2 px-2 py-1 text-[11.5px]">
          <button
            type="button"
            onClick={fit}
            className="rounded-[5px] px-1.5 py-0.5 text-ink-muted transition-colors hover:text-ink"
            title="适配窗口（也可双击画布）"
          >
            适配窗口
          </button>
          <span className="text-ink-faint">·</span>
          <span className="text-ink-faint">拖拽移动 · 滚轮缩放 · 点击查看详情</span>
        </div>
        <div className="glass-panel pointer-events-auto flex flex-wrap items-center gap-x-2.5 gap-y-1 px-2 py-1 text-[11px] text-ink-muted">
          {Object.entries(MEMORY_KIND_LABELS).map(([kind, label]) => (
            <span key={kind} className="flex items-center gap-1">
              <span
                className="inline-block h-2 w-2 rounded-full"
                style={{ backgroundColor: `rgb(var(${kindColorVar(kind)}))` }}
              />
              {label}
            </span>
          ))}
          <span className="flex items-center gap-1">
            <span className="inline-block h-2 w-2 rounded-full border border-warning" />
            置顶
          </span>
        </div>
      </div>

      {pulseNow && (
        <div className="pointer-events-none absolute right-3 top-3 rounded-[6px] border border-accent/40 bg-accent/10 px-2 py-1 text-[11.5px] text-accent">
          正在点亮本次召回的 {litNodes.length} 条记忆
        </div>
      )}

      {nodes.length === 0 && (
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
          <p className="text-[13px] text-ink-faint">还没有可显示的记忆节点</p>
        </div>
      )}
    </div>
  )
}

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0
  return v < 0 ? 0 : v > 1 ? 1 : v
}
