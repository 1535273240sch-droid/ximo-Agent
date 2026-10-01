import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { MemoryGraphEdge, MemoryGraphNode, MemoryNodeDetail } from '@shared/types'
import { MemoryDetailPanel } from './MemoryDetailPanel'

/**
 * 节点详情面板的组件测试（@testing-library/react，jsdom）。
 *
 * 只测「用户能看到什么、点了会发生什么」，不测 Canvas 像素（审核文档 4.9 第 7 条）。
 * 所有数据在本文件构造；回调用 vi.fn()，因此不需要 IPC 桥。
 *
 * 必须显式 cleanup：vitest 的 globals 是关的（vitest.config.ts），
 * @testing-library/react 的自动清理只在全局 afterEach 存在时注册。少了这一句，
 * 上一个用例的 DOM 会留在 document 里，后续 getByText 全部命中多个元素。
 */
afterEach(cleanup)

function node(partial: Partial<MemoryGraphNode> & { id: string }): MemoryGraphNode {
  return {
    kind: 'fact',
    importance: 0.6,
    pinned: false,
    status: 'active',
    use_count: 0,
    degree: 0,
    created_at: 1_700_000_000_000,
    ...partial
  }
}

const SELF = node({ id: 'n1', title: 'Go 并发偏好', content_preview: '偏好 channel', degree: 2, use_count: 7, last_used: 1_700_000_500_000, source_run: 'run-abc' })
const OTHER = node({ id: 'n2', kind: 'entity', title: 'Go', degree: 1 })
const THIRD = node({ id: 'n3', kind: 'topic', title: '后端工程', degree: 1 })

const EDGES: MemoryGraphEdge[] = [
  { src: 'n1', dst: 'n2', rel: 'mentions', weight: 0.5, effective_weight: 0.4 },
  { src: 'n3', dst: 'n1', rel: 'related', weight: 0.2, effective_weight: 0.05 }
]

const DETAIL: MemoryNodeDetail = {
  node: SELF,
  content: '用户明确偏好 channel 而不是 mutex',
  edges: EDGES,
  neighbors: [OTHER, THIRD],
  recalls: [{ run_id: 'run-abc', via: 'seed', used: true, ts: 1_700_000_600_000 }]
}

function setup(detail: MemoryNodeDetail | null = DETAIL) {
  const onSelect = vi.fn()
  const onUpdate = vi.fn(async () => undefined)
  const onDelete = vi.fn(async () => undefined)
  const onLink = vi.fn(async () => undefined)
  render(
    <MemoryDetailPanel
      detail={detail}
      candidates={[SELF, OTHER, THIRD]}
      onSelect={onSelect}
      onUpdate={onUpdate}
      onDelete={onDelete}
      onLink={onLink}
    />
  )
  return { onSelect, onUpdate, onDelete, onLink }
}

describe('MemoryDetailPanel 展示', () => {
  it('空详情时提示去图里点一个节点', () => {
    render(
      <MemoryDetailPanel
        detail={null}
        candidates={[]}
        onSelect={vi.fn()}
        onUpdate={vi.fn(async () => undefined)}
        onDelete={vi.fn(async () => undefined)}
        onLink={vi.fn(async () => undefined)}
      />
    )
    expect(screen.getByText(/点一个节点/)).toBeTruthy()
  })

  it('显示完整正文、用过次数、来源 run 与召回历史', () => {
    setup()
    expect(screen.getByText('Go 并发偏好')).toBeTruthy()
    expect(screen.getByText('用户明确偏好 channel 而不是 mutex')).toBeTruthy()
    expect(screen.getByText('7 次')).toBeTruthy()
    // run id 会出现两次：来源 run 与召回记录里的 run_id，两处都该有。
    expect(screen.getAllByText('run-abc').length).toBe(2)
    expect(screen.getByText(/↳ seed/)).toBeTruthy()
    expect(screen.getByText('已采用')).toBeTruthy()
  })

  it('关联节点带权重条，且数值用有效权重而不是原始权重', () => {
    setup()
    // n1→n2 的 weight=0.5 / effective_weight=0.4
    expect(screen.getByText('0.40')).toBeTruthy()
    // n3→n1 的 weight=0.2 / effective_weight=0.05
    expect(screen.getByText('0.05')).toBeTruthy()
    expect(screen.getByText('→ 提及')).toBeTruthy()
    expect(screen.getByText('← 相关')).toBeTruthy()
  })

  it('点关联节点会请求切换选中项', () => {
    const { onSelect } = setup()
    // 用按钮的可访问名精确定位，避免与「Go 并发偏好」这个标题混淆。
    fireEvent.click(screen.getByRole('button', { name: /→ 提及\s*Go/ }))
    expect(onSelect).toHaveBeenCalledWith('n2')
  })
})

describe('MemoryDetailPanel 操作', () => {
  it('置顶走 update 的指针三态（只带 pinned）', async () => {
    const { onUpdate } = setup()
    fireEvent.click(screen.getByText('置顶'))
    await waitFor(() => expect(onUpdate).toHaveBeenCalledWith({ node_id: 'n1', pinned: true }))
  })

  it('归档把 status 改成 archived', async () => {
    const { onUpdate } = setup()
    fireEvent.click(screen.getByText('归档'))
    await waitFor(() => expect(onUpdate).toHaveBeenCalledWith({ node_id: 'n1', status: 'archived' }))
  })

  it('遗忘必须二次确认才真的删除', async () => {
    const { onDelete } = setup()
    fireEvent.click(screen.getByText('遗忘'))
    // 第一次点击只展开警告，不调用删除。
    expect(onDelete).not.toHaveBeenCalled()
    expect(screen.getByText(/无法撤销/)).toBeTruthy()

    fireEvent.click(screen.getByText('确认遗忘'))
    await waitFor(() => expect(onDelete).toHaveBeenCalledWith('n1'))
  })

  it('编辑后保存把标题/正文/重要度一起提交', async () => {
    const { onUpdate } = setup()
    fireEvent.click(screen.getByText('编辑'))

    fireEvent.change(screen.getByLabelText('标题'), { target: { value: '新标题' } })
    fireEvent.change(screen.getByLabelText('正文'), { target: { value: '新正文' } })
    fireEvent.change(screen.getByLabelText('重要度'), { target: { value: '0.9' } })
    fireEvent.click(screen.getByText('保存'))

    await waitFor(() =>
      expect(onUpdate).toHaveBeenCalledWith({
        node_id: 'n1',
        title: '新标题',
        content: '新正文',
        importance: 0.9
      })
    )
  })

  it('手动连线把 src/dst/rel 交给回调，并排除自己与已连上的节点', async () => {
    const onLink = vi.fn(async () => undefined)
    // 这个场景里 n1 只与 n2 相连：候选应只剩 n3（且不含自己）。
    render(
      <MemoryDetailPanel
        detail={{ ...DETAIL, edges: [EDGES[0]], neighbors: [OTHER] }}
        candidates={[SELF, OTHER, THIRD]}
        onSelect={vi.fn()}
        onUpdate={vi.fn(async () => undefined)}
        onDelete={vi.fn(async () => undefined)}
        onLink={onLink}
      />
    )

    fireEvent.click(screen.getByText('手动连线'))
    const target = screen.getByLabelText('连接到') as HTMLSelectElement
    expect(Array.from(target.options).map((o) => o.value)).toEqual(['', 'n3'])

    fireEvent.change(target, { target: { value: 'n3' } })
    fireEvent.change(screen.getByLabelText('关系'), { target: { value: 'causes' } })
    fireEvent.click(screen.getByText('建立'))

    await waitFor(() => expect(onLink).toHaveBeenCalledWith('n1', 'n3', 'causes'))
  })

  it('没有可连线目标时禁用「手动连线」', () => {
    render(
      <MemoryDetailPanel
        detail={{ ...DETAIL, edges: EDGES, neighbors: [OTHER, THIRD] }}
        candidates={[SELF, OTHER, THIRD]}
        onSelect={vi.fn()}
        onUpdate={vi.fn(async () => undefined)}
        onDelete={vi.fn(async () => undefined)}
        onLink={vi.fn(async () => undefined)}
      />
    )
    const btn = screen.getByText('手动连线').closest('button') as HTMLButtonElement
    expect(btn.disabled).toBe(true)
  })

  it('写操作抛错时把错误显示出来，而不是静默失败', async () => {
    const onUpdate = vi.fn(async () => {
      throw new Error('importance must be within [0,1]')
    })
    render(
      <MemoryDetailPanel
        detail={DETAIL}
        candidates={[SELF, OTHER, THIRD]}
        onSelect={vi.fn()}
        onUpdate={onUpdate}
        onDelete={vi.fn(async () => undefined)}
        onLink={vi.fn(async () => undefined)}
      />
    )
    fireEvent.click(screen.getByText('置顶'))
    expect(await screen.findByText('importance must be within [0,1]')).toBeTruthy()
  })

  it('切换节点会重置编辑/确认态（不会带着上一个节点的草稿）', async () => {
    const { rerender } = render(
      <MemoryDetailPanel
        detail={DETAIL}
        candidates={[SELF, OTHER, THIRD]}
        onSelect={vi.fn()}
        onUpdate={vi.fn(async () => undefined)}
        onDelete={vi.fn(async () => undefined)}
        onLink={vi.fn(async () => undefined)}
      />
    )
    fireEvent.click(screen.getByText('编辑'))
    expect(screen.getByLabelText('标题')).toBeTruthy()

    rerender(
      <MemoryDetailPanel
        detail={{ node: OTHER, content: '另一条', edges: [], neighbors: [] }}
        candidates={[SELF, OTHER, THIRD]}
        onSelect={vi.fn()}
        onUpdate={vi.fn(async () => undefined)}
        onDelete={vi.fn(async () => undefined)}
        onLink={vi.fn(async () => undefined)}
      />
    )
    expect(screen.queryByLabelText('标题')).toBeNull()
    expect(screen.getByText('另一条')).toBeTruthy()
  })
})
