import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { WorkLogStep } from './WorkLogStep'
import type { RunStep } from '../../../store/steps'

/**
 * Work Log 与记忆页的联动（审核文档 4.9 第 5 条）。
 *
 * 只测这一条链路：展开「回忆 N 条记忆」步骤 → 点某条记忆 → 回调拿到节点 id。
 * 之所以值得单独测：这是两个页面的接缝，接缝断了不会报错，只会表现为
 * 「点了没反应」——正是最难被发现的失败形态。
 */
afterEach(cleanup)

const STEP: RunStep = {
  id: 'memory:7',
  kind: 'memory',
  title: '回忆 2 条相关记忆',
  status: 'done',
  startedAt: 1_700_000_000_000,
  endedAt: 1_700_000_000_100,
  detail: {
    recalled: [
      { id: 'mn_1', text: '偏好 channel', via: 'seed' },
      { id: 'mn_2', text: '用 SQLite', via: 'entity:Go → fact:存储' }
    ]
  }
}

describe('WorkLogStep 记忆联动', () => {
  it('展开后点某条记忆，把该节点 id 交给回调', () => {
    const onOpenMemoryNode = vi.fn()
    render(<WorkLogStep step={STEP} onOpenMemoryNode={onOpenMemoryNode} />)

    // 展开步骤详情
    fireEvent.click(screen.getByRole('button', { expanded: false }))
    expect(screen.getByText('偏好 channel')).toBeTruthy()

    fireEvent.click(screen.getByText('偏好 channel'))
    expect(onOpenMemoryNode).toHaveBeenCalledWith('mn_1')

    fireEvent.click(screen.getByText('用 SQLite'))
    expect(onOpenMemoryNode).toHaveBeenCalledWith('mn_2')
  })

  it('没有回调时退化为纯文本，不产生可点区域', () => {
    render(<WorkLogStep step={STEP} />)
    fireEvent.click(screen.getByRole('button', { expanded: false }))
    expect(screen.getByText('偏好 channel').closest('button')).toBeNull()
  })
})
