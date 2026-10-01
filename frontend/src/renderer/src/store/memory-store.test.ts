import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type {
  MemoryExport,
  MemoryGraph,
  MemoryGraphNode,
  MemoryNodeDetail,
  MemoryStats
} from '@shared/types'
import {
  filterNodes,
  edgeWidth,
  exportNodeKey,
  nodeRadius,
  nodeToExportNode,
  parseMemoryExport,
  pruneEdges,
  neighborhood,
  setMemoryBridge,
  toExportDoc,
  useMemoryStore,
  type MemoryBridge
} from './memory-store'

/**
 * memory-store 的单测。
 *
 * 数据全部由本文件构造（生产路径不内置假数据）。桥用 setMemoryBridge 注入，
 * 这样 store 的完整动作链（装载 → 筛选 → 聚焦 → 写操作 → 重新装载）都能被覆盖，
 * 不需要渲染任何组件。
 */

function node(partial: Partial<MemoryGraphNode> & { id: string }): MemoryGraphNode {
  return {
    kind: 'fact',
    importance: 0.5,
    pinned: false,
    status: 'active',
    use_count: 0,
    degree: 0,
    created_at: 1_700_000_000_000,
    ...partial
  }
}

const GRAPH: MemoryGraph = {
  nodes: [
    node({ id: 'n1', title: 'Go 并发偏好', content_preview: '用户偏好 channel 而不是 mutex', degree: 2, importance: 0.8 }),
    node({ id: 'n2', kind: 'entity', title: 'Go', degree: 2, importance: 0.6 }),
    node({ id: 'n3', kind: 'topic', title: '后端工程', degree: 1, importance: 0.4, status: 'archived' }),
    // n4 不在图里，用于验证「幽灵边」被剪掉
    node({ id: 'n5', kind: 'procedure', title: '发布流程', degree: 0, importance: 0.3 })
  ],
  edges: [
    { src: 'n1', dst: 'n2', rel: 'mentions', weight: 0.5, effective_weight: 0.5 },
    { src: 'n2', dst: 'n3', rel: 'related', weight: 0.2, effective_weight: 0.1 },
    { src: 'n1', dst: 'n4', rel: 'related', weight: 0.2, effective_weight: 0.2 }
  ],
  total: 5,
  truncated: true
}

const STATS: MemoryStats = {
  nodes: 5,
  edges: 3,
  recall_calls: 7,
  recall_errors: 0,
  recall_chars: 120,
  bg_dropped: 0,
  consolidations: 1,
  merged_facts: 0,
  archived_nodes: 1,
  topics_created: 0,
  backend: 'synapse',
  enabled: true
}

function detailFor(id: string): MemoryNodeDetail {
  const n = GRAPH.nodes.find((x) => x.id === id) ?? node({ id })
  return {
    node: n,
    content: `完整正文 ${id}`,
    edges: GRAPH.edges.filter((e) => e.src === id || e.dst === id),
    neighbors: GRAPH.nodes.filter((x) =>
      GRAPH.edges.some(
        (e) => (e.src === id && e.dst === x.id) || (e.dst === id && e.src === x.id)
      )
    ),
    recalls: [{ run_id: 'run-1', via: 'seed', used: true, ts: 1_700_000_100_000 }]
  }
}

function fakeBridge(overrides: Partial<MemoryBridge> = {}): MemoryBridge {
  return {
    memoryGraph: vi.fn(async () => GRAPH),
    memoryNode: vi.fn(async (id: string) => detailFor(id)),
    memoryUpdateNode: vi.fn(async (upd) => detailFor(upd.node_id)),
    memoryDeleteNode: vi.fn(async () => undefined),
    memoryLink: vi.fn(async () => ({ ok: true, affected: 1 })),
    memoryConsolidate: vi.fn(async () => ({ ok: true, affected: 3, notes: ['合并 3 条'] })),
    memoryExport: vi.fn(
      async (): Promise<MemoryExport> => ({
        version: 1,
        exported_at: 1_700_000_000_000,
        nodes: GRAPH.nodes.map((n) => nodeToExportNode(n, `完整正文 ${n.id}`)),
        edges: GRAPH.edges
      })
    ),
    memoryImport: vi.fn(async () => ({ ok: true, affected: 4 })),
    memoryStats: vi.fn(async () => STATS),
    memoryClear: vi.fn(async () => ({ ok: true, affected: 5 })),
    ...overrides
  }
}

/** 重置 store 到初始状态（zustand 的 setState 支持部分更新）。 */
function resetStore(): void {
  useMemoryStore.setState({
    nodes: [],
    edges: [],
    total: 0,
    truncated: false,
    detail: null,
    stats: null,
    mode: 'graph',
    kinds: [],
    listKinds: [],
    query: '',
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
    reducedMotion: false
  })
}

beforeEach(() => {
  resetStore()
  setMemoryBridge(fakeBridge())
})

afterEach(() => {
  setMemoryBridge(undefined)
  useMemoryStore.getState().stopRecallAnimation()
  vi.useRealTimers()
})

describe('memory-store 装载', () => {
  it('loadGraph 落地节点、剪掉指向图外的幽灵边、保留 total/truncated', async () => {
    await useMemoryStore.getState().loadGraph()
    const s = useMemoryStore.getState()
    expect(s.nodes.map((n) => n.id)).toEqual(['n1', 'n2', 'n3', 'n5'])
    // n1→n4 的边必须被剪掉：n4 不在本次返回的节点里，画出来就是指向画布外。
    expect(s.edges.map((e) => `${e.src}->${e.dst}`)).toEqual(['n1->n2', 'n2->n3'])
    expect(s.total).toBe(5)
    expect(s.truncated).toBe(true)
    expect(s.loading).toBe(false)
    expect(s.error).toBeNull()
  })

  it('loadGraph 把 kinds 与 include_archived 传进请求', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    useMemoryStore.setState({ kinds: ['fact'], includeArchived: true })
    await useMemoryStore.getState().loadGraph()
    expect(bridge.memoryGraph).toHaveBeenCalledWith(
      expect.objectContaining({ kinds: ['fact'], include_archived: true })
    )
  })

  it('loadGraph 在 query 非空时带上 query 并算出搜索命中', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    useMemoryStore.setState({ query: '并发' })
    await useMemoryStore.getState().loadGraph()
    expect(bridge.memoryGraph).toHaveBeenCalledWith(expect.objectContaining({ query: '并发' }))
    expect(useMemoryStore.getState().searchHits).toEqual(['n1'])
  })

  it('loadGraph 在 centerOn 时走邻域查询', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    await useMemoryStore.getState().loadGraph({ centerOn: 'n2', depth: 2 })
    expect(bridge.memoryGraph).toHaveBeenCalledWith(
      expect.objectContaining({ center_on: 'n2', depth: 2 })
    )
  })

  it('没有桥时给出明确错误，而不是永远 loading', async () => {
    setMemoryBridge(undefined)
    // 清掉 window.ximo（jsdom 里默认不存在）
    delete (window as unknown as { ximo?: unknown }).ximo
    await useMemoryStore.getState().loadGraph()
    const s = useMemoryStore.getState()
    expect(s.loading).toBe(false)
    expect(s.error).toContain('记忆服务不可用')
  })

  it('loadStats 失败不会清掉已有的图数据', async () => {
    await useMemoryStore.getState().loadGraph()
    setMemoryBridge(
      fakeBridge({
        memoryStats: vi.fn(async () => {
          throw new Error('boom')
        })
      })
    )
    await useMemoryStore.getState().loadStats()
    const s = useMemoryStore.getState()
    expect(s.error).toBe('boom')
    expect(s.nodes.length).toBe(4)
  })
})

describe('memory-store 筛选与聚焦', () => {
  it('filterNodes 按 kind / 状态 / 搜索词过滤', () => {
    expect(filterNodes(GRAPH.nodes, { kinds: ['entity'] }).map((n) => n.id)).toEqual(['n2'])
    expect(filterNodes(GRAPH.nodes, { status: 'active' }).map((n) => n.id)).toEqual([
      'n1',
      'n2',
      'n5'
    ])
    expect(filterNodes(GRAPH.nodes, { query: '后端' }).map((n) => n.id)).toEqual(['n3'])
    expect(filterNodes(GRAPH.nodes)).toHaveLength(4)
  })

  it('pruneEdges 只保留两端都在集合里的边', () => {
    const kept = pruneEdges(GRAPH.edges, new Set(['n1', 'n2', 'n3']))
    expect(kept).toHaveLength(2)
  })

  it('neighborhood 返回 depth 跳内的节点（无向遍历）', () => {
    // 注意：GRAPH.edges 里有 n1→n4 这条边（n4 不在节点列表里，用于测剪边）。
    // 邻域计算只关心边本身，所以 2 跳会带出 n4——这正是「先剪边再算邻域」的
    // 调用顺序在组件侧必须成立的原因。
    expect([...neighborhood(GRAPH.edges, 'n1', 1)].sort()).toEqual(['n1', 'n2', 'n4'])
    expect([...neighborhood(GRAPH.edges, 'n1', 2)].sort()).toEqual(['n1', 'n2', 'n3', 'n4'])
    // 孤立节点只返回自己。
    expect([...neighborhood(GRAPH.edges, 'n5', 2)]).toEqual(['n5'])
  })

  it('selectNode 落地详情，且快速切换时只接受最后一次响应', async () => {
    // 用对象包一层存 resolver：直接写 `let resolveFirst: ((d) => void) | null = null`
    // 会被 TS 的控制流分析窄化成 never（赋值只发生在 Promise 执行器里）。
    const pending: { resolve?: (d: MemoryNodeDetail) => void } = {}
    const bridge = fakeBridge({
      memoryNode: vi.fn((id: string) => {
        if (id === 'n1') {
          return new Promise<MemoryNodeDetail>((res) => {
            pending.resolve = res
          })
        }
        return Promise.resolve(detailFor(id))
      })
    })
    setMemoryBridge(bridge)

    const p1 = useMemoryStore.getState().selectNode('n1')
    const p2 = useMemoryStore.getState().selectNode('n2')
    await p2
    // 后到的 n1 响应必须被丢弃，否则详情面板会显示与选中项不一致的节点。
    pending.resolve?.(detailFor('n1'))
    await p1
    expect(useMemoryStore.getState().detail?.node.id).toBe('n2')
    expect(useMemoryStore.getState().selectedId).toBe('n2')
  })

  it('focusNode 记录 id 并自增 focusSeq（同一节点再次聚焦也要生效）', () => {
    useMemoryStore.getState().focusNode('n3')
    expect(useMemoryStore.getState().focusNodeId).toBe('n3')
    const seq1 = useMemoryStore.getState().focusSeq
    useMemoryStore.getState().focusNode('n3')
    expect(useMemoryStore.getState().focusSeq).toBe(seq1 + 1)
    useMemoryStore.getState().focusNode(null)
    expect(useMemoryStore.getState().focusNodeId).toBeNull()
  })
})

describe('memory-store 写操作', () => {
  it('updateNode 落详情、给提示并重新装载图', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    await useMemoryStore.getState().updateNode({ node_id: 'n1', pinned: true })
    expect(bridge.memoryUpdateNode).toHaveBeenCalledWith({ node_id: 'n1', pinned: true })
    expect(useMemoryStore.getState().notice).toBe('已保存')
    expect(bridge.memoryGraph).toHaveBeenCalled()
  })

  it('deleteNode 清掉被删节点的选中态并刷新', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    await useMemoryStore.getState().selectNode('n1')
    await useMemoryStore.getState().deleteNode('n1')
    expect(bridge.memoryDeleteNode).toHaveBeenCalledWith('n1')
    expect(useMemoryStore.getState().selectedId).toBeNull()
    expect(useMemoryStore.getState().detail).toBeNull()
    expect(useMemoryStore.getState().notice).toContain('已遗忘')
  })

  it('clearAll 把确认字面量原样透传（不替用户填）', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    await useMemoryStore.getState().clearAll('DELETE_ALL')
    expect(bridge.memoryClear).toHaveBeenCalledWith('DELETE_ALL')
  })

  it('写操作失败时把错误暴露给界面且不吞掉异常', async () => {
    setMemoryBridge(
      fakeBridge({
        memoryDeleteNode: vi.fn(async () => {
          throw new Error('node not found')
        })
      })
    )
    await expect(useMemoryStore.getState().deleteNode('n9')).rejects.toThrow('node not found')
    expect(useMemoryStore.getState().error).toBe('node not found')
  })
})

describe('导出 → 导入 载荷往返', () => {
  it('导出节点的形状与 Go 侧 MemoryExportNode 一致，往返不丢数据', async () => {
    const doc = await useMemoryStore.getState().exportAll()
    expect(doc.version).toBe(1)
    expect(doc.nodes).toHaveLength(4)
    // 逐字段对齐 Go 的 JSON tag（snake_case）
    expect(Object.keys(doc.nodes[0]).sort()).toEqual(
      ['content', 'created_at', 'id', 'importance', 'kind', 'pinned', 'source_run', 'status', 'title', 'use_count'].sort()
    )

    const json = JSON.stringify(doc)
    const back = parseMemoryExport(JSON.parse(json))
    expect(back.nodes.map(exportNodeKey)).toEqual(doc.nodes.map(exportNodeKey))
    expect(back.edges).toHaveLength(doc.edges.length)
  })

  it('toExportDoc 保留完整正文与边', () => {
    const doc = toExportDoc(
      [nodeToExportNode(GRAPH.nodes[0], '完整正文 n1')],
      [GRAPH.edges[0]],
      'u1'
    )
    expect(doc.nodes[0].content).toBe('完整正文 n1')
    expect(doc.user_id).toBe('u1')
    expect(doc.edges[0].effective_weight).toBe(0.5)
  })

  it('parseMemoryExport 拒绝缺 nodes 的载荷并补齐缺省字段', () => {
    expect(() => parseMemoryExport({})).toThrow(/没有 nodes/)
    expect(() => parseMemoryExport(null)).toThrow(/JSON 对象/)
    expect(() => parseMemoryExport({ nodes: [{ noId: true }] })).toThrow(/可用的节点/)

    const fixed = parseMemoryExport({ nodes: [{ id: 'x', content: 'c' }] })
    expect(fixed.nodes[0]).toMatchObject({
      id: 'x',
      kind: 'fact',
      content: 'c',
      importance: 0.5,
      pinned: false,
      status: 'active',
      use_count: 0
    })
  })

  it('importDoc 走桥并刷新', async () => {
    const bridge = fakeBridge()
    setMemoryBridge(bridge)
    const doc = await useMemoryStore.getState().exportAll()
    await useMemoryStore.getState().importDoc(doc)
    expect(bridge.memoryImport).toHaveBeenCalledWith(doc)
    expect(useMemoryStore.getState().notice).toContain('已导入 4 条')
  })
})

describe('memory.recalled 点亮动画', () => {
  it('点亮被召回的节点，并让它们之间的边流光', async () => {
    vi.useFakeTimers()
    await useMemoryStore.getState().loadGraph()
    useMemoryStore.getState().triggerRecall([
      { id: 'n1', text: 'a', via: 'seed' },
      { id: 'n2', text: 'b', via: 'n1→n2' }
    ])
    const s = useMemoryStore.getState()
    expect(s.litNodes).toEqual(['n1', 'n2'])
    expect(s.litEdges).toEqual(['n1\u0000n2'])
    // 依次脉冲：第二条比第一条晚一个节拍。
    expect(s.pulses.map((p) => p.delayMs)).toEqual([0, 140])

    vi.advanceTimersByTime(140 + 1400)
    expect(useMemoryStore.getState().litNodes).toEqual([])
    expect(useMemoryStore.getState().litEdges).toEqual([])
  })

  it('只有一端被召回的边不会流光', async () => {
    await useMemoryStore.getState().loadGraph()
    useMemoryStore.getState().triggerRecall([{ id: 'n1' }])
    expect(useMemoryStore.getState().litNodes).toEqual(['n1'])
    expect(useMemoryStore.getState().litEdges).toEqual([])
  })

  it('空 items 直接清空点亮集合', () => {
    useMemoryStore.getState().triggerRecall([])
    expect(useMemoryStore.getState().litNodes).toEqual([])
  })

  it('stopRecallAnimation 立刻熄灭并清掉定时器', () => {
    vi.useFakeTimers()
    useMemoryStore.getState().triggerRecall([{ id: 'n1' }])
    useMemoryStore.getState().stopRecallAnimation()
    expect(useMemoryStore.getState().litNodes).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
  })
})

describe('图视图的几何纯函数', () => {
  it('nodeRadius 随 importance 与度数单调增大', () => {
    const small = nodeRadius({ importance: 0, degree: 0 })
    const big = nodeRadius({ importance: 1, degree: 25 })
    expect(small).toBeGreaterThan(0)
    expect(big).toBeGreaterThan(small)
  })

  it('edgeWidth 按 effective_weight（不是 weight）', () => {
    const fresh = edgeWidth({ effective_weight: 1 })
    const stale = edgeWidth({ effective_weight: 0.02 })
    expect(fresh).toBeGreaterThan(stale)
    // 越界与 NaN 都要被夹住，否则线宽会变成 NaN，整条边静默消失。
    expect(Number.isFinite(edgeWidth({ effective_weight: Number.NaN }))).toBe(true)
    expect(edgeWidth({ effective_weight: 99 })).toBeLessThanOrEqual(4)
  })
})
