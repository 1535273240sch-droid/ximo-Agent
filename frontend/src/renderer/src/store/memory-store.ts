/**
 * 记忆页的数据层（P1-c，审核文档 4.9）。
 *
 * 为什么单独一个 store 而不是复用 app-store：
 *   1. 记忆是跨 run 的长期资产，它的生命周期与「当前会话/当前 run」无关。挂进
 *      app-store 会让每次 run 事件都经过一份无关的大对象比较。
 *   2. app-store 由 Lead 持有（既有功能），记忆页的写入面必须与之隔离——这里只
 *      暴露一个 focusNodeId 作为联动接口，而不是往 app-store 里塞记忆字段。
 *
 * 数据来源只有一个：window.ximo 的记忆桥（经 setMemoryBridge 注入）。生产路径
 * 没有任何内置假数据；单测用 setMemoryBridge 注入自己的假桥。
 *
 * 筛选/聚焦/点亮这类纯计算全部导出成纯函数，便于直接断言（组件测试不碰 Canvas）。
 */

import { create } from 'zustand'
import type {
  MemoryExport,
  MemoryExportNode,
  MemoryGraph,
  MemoryGraphEdge,
  MemoryGraphNode,
  MemoryKind,
  MemoryLink,
  MemoryNodeDetail,
  MemoryNodeUpdate,
  MemoryStats
} from '@shared/types'

/**
 * 记忆页用到的桥接面。
 *
 * 只声明用到的十个方法，而不是直接引用 XimoBridge：单测里注入的假桥只需要实现
 * 这些，不必伪造整个应用桥（伪造越少，测试越不容易因为无关改动而失效）。
 */
export interface MemoryBridge {
  memoryGraph(req: {
    limit?: number
    offset?: number
    kinds?: MemoryKind[]
    include_archived?: boolean
    query?: string
    center_on?: string
    depth?: number
  }): Promise<MemoryGraph>
  memoryNode(id: string): Promise<MemoryNodeDetail>
  memoryUpdateNode(upd: MemoryNodeUpdate): Promise<MemoryNodeDetail>
  memoryDeleteNode(id: string): Promise<void>
  memoryLink(link: MemoryLink): Promise<unknown>
  memoryConsolidate(): Promise<unknown>
  memoryExport(): Promise<MemoryExport>
  memoryImport(doc: MemoryExport): Promise<unknown>
  memoryStats(): Promise<MemoryStats>
  memoryClear(confirm: string): Promise<unknown>
}

/** 从全局 window.ximo 取桥；不存在时返回 undefined（单测环境没有 preload）。 */
export function bridgeFromWindow(): MemoryBridge | undefined {
  if (typeof window === 'undefined') return undefined
  const w = window as unknown as { ximo?: Partial<MemoryBridge> }
  const b = w.ximo
  if (!b || typeof b.memoryGraph !== 'function') return undefined
  return b as MemoryBridge
}

// ---------------------------------------------------------------------------
// 纯函数：筛选 / 聚焦 / 统计 / 导出往返
// ---------------------------------------------------------------------------

/** 记忆页的视图模式。 */
export type MemoryViewMode = 'graph' | 'list'

/** 图视图的加载上限：超过这个数只加载最重要的那批（界面会提示还有多少未加载）。 */
export const GRAPH_PAGE_LIMIT = 600

/** 列表视图一次加载的节点数。 */
export const LIST_PAGE_LIMIT = 500

/** 纯函数：按 kind / 状态 / 搜索词过滤节点。 */
export function filterNodes(
  nodes: MemoryGraphNode[],
  opts: { kinds?: string[]; query?: string; status?: string } = {}
): MemoryGraphNode[] {
  const kinds = opts.kinds && opts.kinds.length > 0 ? new Set(opts.kinds) : undefined
  const q = (opts.query ?? '').trim().toLowerCase()
  return nodes.filter((n) => {
    if (kinds && !kinds.has(n.kind)) return false
    if (opts.status && n.status !== opts.status) return false
    if (q) {
      const hay = `${n.title ?? ''}\n${n.content_preview ?? ''}`.toLowerCase()
      if (!hay.includes(q)) return false
    }
    return true
  })
}

/**
 * 纯函数：只保留两端都在给定节点集合里的边。
 *
 * 这是必需的，不是优化：Graph 接口返回的边可能指向被 Limit 截掉的节点，
 * 直接画会得到一堆指向画布外的"幽灵边"。
 */
export function pruneEdges(edges: MemoryGraphEdge[], nodeIds: Set<string>): MemoryGraphEdge[] {
  return edges.filter((e) => nodeIds.has(e.src) && nodeIds.has(e.dst))
}

/**
 * 纯函数：取某个节点的邻域（depth 跳）。
 *
 * 返回节点 id 集合，供「聚焦该节点」时高亮它和它的邻居。
 */
export function neighborhood(
  edges: MemoryGraphEdge[],
  centerId: string,
  depth = 1
): Set<string> {
  const seen = new Set<string>([centerId])
  let frontier = new Set<string>([centerId])
  for (let d = 0; d < depth; d++) {
    const next = new Set<string>()
    for (const e of edges) {
      if (frontier.has(e.src) && !seen.has(e.dst)) {
        seen.add(e.dst)
        next.add(e.dst)
      }
      if (frontier.has(e.dst) && !seen.has(e.src)) {
        seen.add(e.src)
        next.add(e.src)
      }
    }
    if (next.size === 0) break
    frontier = next
  }
  return seen
}

/** 纯函数：节点在画布上的半径（importance 与度数共同决定，与 Go 侧 Degree 注释一致）。 */
export function nodeRadius(n: Pick<MemoryGraphNode, 'importance' | 'degree'>): number {
  const base = 4 + clamp01(n.importance) * 6
  const degreeBonus = Math.min(6, Math.sqrt(Math.max(0, n.degree)) * 1.4)
  return base + degreeBonus
}

function clamp01(v: number): number {
  if (!Number.isFinite(v)) return 0
  return v < 0 ? 0 : v > 1 ? 1 : v
}

/** 纯函数：边在画布上的线宽，按 effective_weight（不是 weight）。 */
export function edgeWidth(e: Pick<MemoryGraphEdge, 'effective_weight'>): number {
  const w = Number.isFinite(e.effective_weight) ? e.effective_weight : 0
  return 0.6 + clamp01(w) * 3.4
}

/** 纯函数：把图节点转成导出节点（保留完整正文；图节点只有预览）。 */
export function toExportDoc(
  nodes: MemoryExportNode[],
  edges: MemoryGraphEdge[],
  userId = ''
): MemoryExport {
  return {
    version: 1,
    exported_at: Date.now(),
    user_id: userId || undefined,
    nodes,
    edges
  }
}

/**
 * 纯函数：把图节点补成导出节点。
 *
 * 图节点只有 content_preview，导出必须带完整正文，所以调用方要先取详情；
 * 这里对拿不到正文的节点用预览兜底，保证「导出→导入」不会丢节点。
 */
export function nodeToExportNode(
  n: MemoryGraphNode,
  content?: string
): MemoryExportNode {
  return {
    id: n.id,
    kind: n.kind,
    title: n.title,
    content: content ?? n.content_preview ?? '',
    importance: n.importance,
    pinned: n.pinned,
    status: n.status,
    source_run: n.source_run,
    use_count: n.use_count,
    created_at: n.created_at
  }
}

/** 纯函数：导出节点的稳定键，用于「导出→导入」往返时校验不丢数据。 */
export function exportNodeKey(n: MemoryExportNode): string {
  return `${n.id}\u0000${n.kind}\u0000${n.content}`
}

/**
 * 纯函数：校验并规整一份待导入的 JSON。
 *
 * 导入的是用户从磁盘选的文件，可能是任意内容。这里必须自己校验，而不是相信
 * `as MemoryExport`：一个缺 nodes 的载荷会让后端返回「import payload has no
 * nodes」，但错误发生在一次 IPC 往返之后，用户看到的是一句模糊的失败。
 */
export function parseMemoryExport(raw: unknown): MemoryExport {
  if (!raw || typeof raw !== 'object') throw new Error('导入文件不是一个 JSON 对象')
  const o = raw as Record<string, unknown>
  const nodes = Array.isArray(o.nodes) ? o.nodes : undefined
  if (!nodes || nodes.length === 0) throw new Error('导入文件里没有 nodes 数组')
  const clean: MemoryExportNode[] = []
  for (const item of nodes) {
    if (!item || typeof item !== 'object') continue
    const n = item as Record<string, unknown>
    if (typeof n.id !== 'string' || n.id === '') continue
    clean.push({
      id: n.id,
      kind: typeof n.kind === 'string' && n.kind !== '' ? n.kind : 'fact',
      title: typeof n.title === 'string' ? n.title : undefined,
      content: typeof n.content === 'string' ? n.content : '',
      importance: typeof n.importance === 'number' ? n.importance : 0.5,
      pinned: n.pinned === true,
      status: typeof n.status === 'string' && n.status !== '' ? n.status : 'active',
      source_run: typeof n.source_run === 'string' ? n.source_run : undefined,
      use_count: typeof n.use_count === 'number' ? n.use_count : 0,
      created_at: typeof n.created_at === 'number' ? n.created_at : Date.now()
    })
  }
  if (clean.length === 0) throw new Error('导入文件里没有可用的节点（缺少 id）')
  const edges = Array.isArray(o.edges)
    ? (o.edges as MemoryGraphEdge[]).filter(
        (e) => e && typeof e.src === 'string' && typeof e.dst === 'string'
      )
    : []
  return {
    version: typeof o.version === 'number' ? o.version : 1,
    exported_at: typeof o.exported_at === 'number' ? o.exported_at : Date.now(),
    user_id: typeof o.user_id === 'string' ? o.user_id : undefined,
    nodes: clean,
    edges
  }
}

// ---------------------------------------------------------------------------
// store
// ---------------------------------------------------------------------------

/** 「点亮」动画里一个节点的时间片。 */
export interface RecallPulse {
  id: string
  /** 该节点开始发光的时间（相对触发时刻的毫秒数），用于「依次脉冲」。 */
  delayMs: number
}

/** 点亮动画的节拍（毫秒）。 */
export const RECALL_STEP_MS = 140
export const RECALL_PULSE_MS = 1400

export interface MemoryState {
  // --- 数据 ---
  nodes: MemoryGraphNode[]
  edges: MemoryGraphEdge[]
  /** 符合条件的节点总数（可能大于已加载的 nodes.length）。 */
  total: number
  truncated: boolean
  /** 当前详情（选中节点）。 */
  detail: MemoryNodeDetail | null
  stats: MemoryStats | null

  // --- 视图状态 ---
  mode: MemoryViewMode
  /** 图视图的 kind 筛选；空数组表示全部。 */
  kinds: MemoryKind[]
  /** 搜索词（图视图用于高亮、列表视图用于过滤）。 */
  query: string
  /** 列表视图的 kind 筛选（与图共用同一份数据，筛选各自独立）。 */
  listKinds: MemoryKind[]
  includeArchived: boolean
  /** 当前选中的节点 id。 */
  selectedId: string | null
  /** 搜索命中的节点 id（图视图据此高亮）。 */
  searchHits: string[]

  // --- 联动 ---
  /**
   * 待聚焦的节点 id（Work Log 里点「回忆到的某条记忆」时写入）。
   *
   * 只放一个 id，不引入路由库：记忆页已经挂在 App 的 view 联合里，聚焦只是
   * 「换到 memory 视图 + 以该节点为中心取邻域」。
   */
  focusNodeId: string | null
  /** 每次 focusNode 调用自增，用于区分「新的一次聚焦」与「同一 id 再次渲染」。 */
  focusSeq: number

  // --- 点亮动画 ---
  /** 正在脉冲发光的节点 id。 */
  litNodes: string[]
  /** 正在流光的一次性边（`src→dst`）。 */
  litEdges: string[]
  /** 本次点亮的排期（含 delay），渲染层据此做「依次」。 */
  pulses: RecallPulse[]

  // --- 状态 ---
  loading: boolean
  error: string | null
  /** 最近一次写操作的结果说明（成功提示，例如「已归档 3 条」）。 */
  notice: string | null
  /** 是否尊重 prefers-reduced-motion（渲染层据此关掉动画）。 */
  reducedMotion: boolean

  // --- 动作 ---
  setMode: (m: MemoryViewMode) => void
  setKinds: (k: MemoryKind[]) => void
  setListKinds: (k: MemoryKind[]) => void
  setQuery: (q: string) => void
  setIncludeArchived: (v: boolean) => void
  setReducedMotion: (v: boolean) => void
  clearNotice: () => void

  loadGraph: (opts?: { centerOn?: string; depth?: number; append?: boolean }) => Promise<void>
  loadStats: () => Promise<void>
  selectNode: (id: string | null) => Promise<void>
  updateNode: (upd: MemoryNodeUpdate) => Promise<void>
  deleteNode: (id: string) => Promise<void>
  linkNodes: (link: MemoryLink) => Promise<void>
  consolidate: () => Promise<void>
  clearAll: (confirm: string) => Promise<void>
  exportAll: () => Promise<MemoryExport>
  importDoc: (doc: MemoryExport) => Promise<void>

  focusNode: (id: string | null) => void
  /** 收到 memory.recalled 事件时点亮被召回的节点与激活路径。 */
  triggerRecall: (items: { id: string; text?: string; via?: string }[]) => void
  /** 停掉点亮动画（切换视图/卸载时调用，避免定时器泄漏）。 */
  stopRecallAnimation: () => void
}

/** 模块级桥引用：不进 state（它不是渲染数据，进 state 会让每次 set 都做一次深比较）。 */
let bridge: MemoryBridge | undefined

/**
 * 注入记忆桥。
 *
 * 生产路径由 MemoryView 挂载时注入 window.ximo；单测注入自己的假桥。
 * 传 undefined 可清空（组件卸载时调用，避免测试之间互相污染）。
 */
export function setMemoryBridge(b: MemoryBridge | undefined): void {
  bridge = b
}

/** 取当前桥；没有桥时抛一个明确的错误，而不是让界面永远转圈。 */
export function requireMemoryBridge(): MemoryBridge {
  const b = bridge ?? bridgeFromWindow()
  if (!b) {
    throw new Error('记忆服务不可用：后端桥未就绪')
  }
  return b
}

/** 当前桥（可为空），供只读探测使用。 */
export function peekMemoryBridge(): MemoryBridge | undefined {
  return bridge ?? bridgeFromWindow()
}

/** 动画定时器句柄（模块级，不参与渲染）。 */
let recallTimer: ReturnType<typeof setTimeout> | null = null

function clearRecallTimer(): void {
  if (recallTimer !== null) {
    clearTimeout(recallTimer)
    recallTimer = null
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

export const useMemoryStore = create<MemoryState>((set, get) => ({
  nodes: [],
  edges: [],
  total: 0,
  truncated: false,
  detail: null,
  stats: null,

  mode: 'graph',
  kinds: [],
  query: '',
  listKinds: [],
  includeArchived: false,
  selectedId: null,
  searchHits: [],

  focusNodeId: null,
  focusSeq: 0,

  litNodes: [],
  litEdges: [],
  pulses: [],

  loading: false,
  error: null,
  notice: null,
  reducedMotion: false,

  setMode: (m) => set({ mode: m }),
  setKinds: (k) => set({ kinds: k }),
  setListKinds: (k) => set({ listKinds: k }),
  setQuery: (q) => set({ query: q }),
  setIncludeArchived: (v) => set({ includeArchived: v }),
  setReducedMotion: (v) => set({ reducedMotion: v }),
  clearNotice: () => set({ notice: null }),

  /**
   * 装载子图。
   *
   * centerOn 非空时走「邻域」查询（聚焦某节点），否则走普通分页查询。两条路径
   * 共用同一段落地逻辑，避免「聚焦后筛选状态丢失」这类分叉 bug。
   */
  loadGraph: async (opts) => {
    const st = get()
    set({ loading: true, error: null })
    try {
      const b = requireMemoryBridge()
      const req: Parameters<MemoryBridge['memoryGraph']>[0] = {
        limit: st.mode === 'list' ? LIST_PAGE_LIMIT : GRAPH_PAGE_LIMIT,
        include_archived: st.includeArchived || undefined
      }
      const kinds = st.mode === 'list' ? st.listKinds : st.kinds
      if (kinds.length > 0) req.kinds = kinds
      if (opts?.centerOn) {
        req.center_on = opts.centerOn
        req.depth = opts.depth ?? 1
      } else if (st.query.trim() !== '') {
        req.query = st.query.trim()
      }
      const g = await b.memoryGraph(req)
      const nodes = Array.isArray(g.nodes) ? g.nodes : []
      const ids = new Set(nodes.map((n) => n.id))
      const edges = pruneEdges(Array.isArray(g.edges) ? g.edges : [], ids)
      // 搜索命中的节点用于高亮：后端在 query 非空时只返回命中项及其直接关联，
      // 这里再按标题/预览做一次本地匹配，把"直接关联"与"真命中"区分开。
      const q = st.query.trim().toLowerCase()
      const searchHits =
        q === ''
          ? []
          : nodes
              .filter((n) =>
                `${n.title ?? ''}\n${n.content_preview ?? ''}`.toLowerCase().includes(q)
              )
              .map((n) => n.id)
      set({
        nodes,
        edges,
        total: typeof g.total === 'number' ? g.total : nodes.length,
        truncated: g.truncated === true,
        searchHits,
        loading: false
      })
    } catch (err) {
      set({ loading: false, error: errText(err) })
    }
  },

  loadStats: async () => {
    try {
      const b = requireMemoryBridge()
      const stats = await b.memoryStats()
      set({ stats })
    } catch (err) {
      // 统计失败不该盖掉图数据：它只是页面角落的一行计数。
      set({ error: errText(err) })
    }
  },

  selectNode: async (id) => {
    if (id === null) {
      set({ selectedId: null, detail: null })
      return
    }
    set({ selectedId: id, error: null })
    try {
      const b = requireMemoryBridge()
      const detail = await b.memoryNode(id)
      // 竞态保护：用户快速点了两个节点时，只接受最后选中的那个的响应。
      if (get().selectedId !== id) return
      set({ detail })
    } catch (err) {
      if (get().selectedId !== id) return
      set({ error: errText(err), detail: null })
    }
  },

  updateNode: async (upd) => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      const detail = await b.memoryUpdateNode(upd)
      set({ detail, notice: '已保存' })
      await get().loadGraph()
      await get().loadStats()
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  deleteNode: async (id) => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      await b.memoryDeleteNode(id)
      set({
        notice: '已遗忘该节点及其全部关联',
        detail: get().selectedId === id ? null : get().detail,
        selectedId: get().selectedId === id ? null : get().selectedId
      })
      await get().loadGraph()
      await get().loadStats()
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  linkNodes: async (link) => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      const mut = await b.memoryLink(link)
      set({ notice: mutNotes(mut, '已连线') })
      await get().loadGraph()
      if (get().selectedId) await get().selectNode(get().selectedId)
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  consolidate: async () => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      const mut = await b.memoryConsolidate()
      set({ notice: mutNotes(mut, '整理完成') })
      await get().loadGraph()
      await get().loadStats()
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  clearAll: async (confirm) => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      const mut = await b.memoryClear(confirm)
      set({
        notice: mutNotes(mut, '已清空全部记忆'),
        detail: null,
        selectedId: null
      })
      await get().loadGraph()
      await get().loadStats()
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  exportAll: async () => {
    const b = requireMemoryBridge()
    return b.memoryExport()
  },

  importDoc: async (doc) => {
    set({ error: null })
    try {
      const b = requireMemoryBridge()
      const mut = await b.memoryImport(doc)
      set({ notice: mutNotes(mut, `已导入 ${doc.nodes.length} 条` ) })
      await get().loadGraph()
      await get().loadStats()
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  },

  focusNode: (id) => {
    const seq = get().focusSeq + 1
    set({ focusNodeId: id, focusSeq: seq })
  },

  /**
   * 点亮被召回的节点。
   *
   * 输入形状来自 types.EventMemoryRecalled 的 Data：`{count, items:[{id,text,via}]}`。
   * via 是给人看的路径描述（如 "entity:Go → fact:并发偏好"），不是结构化的边表，
   * 所以这里不去解析它——解析一个展示用字符串必然出错。改为：点亮 items 里的
   * 节点，并让「这些节点之间已存在的边」流光一次。语义上仍然是「激活路径」。
   */
  triggerRecall: (items) => {
    clearRecallTimer()
    const ids = items.map((i) => i.id).filter((id) => id !== '')
    if (ids.length === 0) {
      set({ litNodes: [], litEdges: [], pulses: [] })
      return
    }
    const idSet = new Set(ids)
    const litEdges = get()
      .edges.filter((e) => idSet.has(e.src) && idSet.has(e.dst))
      .map((e) => `${e.src}\u0000${e.dst}`)
    const pulses: RecallPulse[] = ids.map((id, i) => ({ id, delayMs: i * RECALL_STEP_MS }))
    set({ litNodes: ids, litEdges, pulses })

    // 依次脉冲的总时长之后统一熄灭；用单个定时器而不是每个节点一个，
    // 避免快速连续召回时留下一堆互相打架的定时器。
    const total = (ids.length - 1) * RECALL_STEP_MS + RECALL_PULSE_MS
    recallTimer = setTimeout(() => {
      recallTimer = null
      set({ litNodes: [], litEdges: [], pulses: [] })
    }, total)
  },

  stopRecallAnimation: () => {
    clearRecallTimer()
    set({ litNodes: [], litEdges: [], pulses: [] })
  }
}))

/** 把写操作的结果压成一行用户可读的提示（有 notes 优先用 notes）。 */
function mutNotes(mut: unknown, fallback: string): string {
  const m = mut as { affected?: number; notes?: string[] } | null
  if (m && Array.isArray(m.notes) && m.notes.length > 0) return m.notes.join('；')
  if (m && typeof m.affected === 'number' && m.affected > 0) {
    return `${fallback}（影响 ${m.affected} 项）`
  }
  return fallback
}

/** 记忆页里 kind 的中文名（图例、列表筛选、详情都共用）。 */export const MEMORY_KIND_LABELS: Record<string, string> = {
  fact: '事实',
  entity: '实体',
  episode: '片段',
  procedure: '流程',
  topic: '主题'
}

/** 边关系的中文名。 */
export const MEMORY_REL_LABELS: Record<string, string> = {
  mentions: '提及',
  related: '相关',
  part_of: '属于',
  causes: '导致',
  derived_from: '衍生自',
  supersedes: '取代',
  contradicts: '矛盾',
  same_topic: '同主题',
  used_with: '共现'
}

/** 节点状态的中文名。 */
export const MEMORY_STATUS_LABELS: Record<string, string> = {
  active: '生效',
  superseded: '已被取代',
  archived: '已归档'
}

/** 一个 kind 对应的强调色 token（Tailwind class）。 */
export function kindAccentClass(kind: string): string {
  switch (kind) {
    case 'fact':
      return 'text-accent'
    case 'entity':
      return 'text-success'
    case 'episode':
      return 'text-warning'
    case 'procedure':
      return 'text-danger'
    case 'topic':
      return 'text-ink'
    default:
      return 'text-ink-muted'
  }
}

/** 一个 kind 对应的画布颜色（CSS 变量名，Canvas 里用 getComputedStyle 解析）。 */
export function kindColorVar(kind: string): string {
  switch (kind) {
    case 'fact':
      return '--c-accent'
    case 'entity':
      return '--c-success'
    case 'episode':
      return '--c-warning'
    case 'procedure':
      return '--c-danger'
    case 'topic':
      return '--c-ink'
    default:
      return '--c-ink-muted'
  }
}

/** 供测试与界面复用的「有效权重」读取（避免各处重复写 fallback）。 */
export function effectiveWeight(e: MemoryGraphEdge): number {
  return Number.isFinite(e.effective_weight) ? e.effective_weight : e.weight
}
