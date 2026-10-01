import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ModelListPayload, XimoBridge } from '@shared/types'
import {
  MODEL_PANEL_WIDTH,
  ModelPicker,
  computePanelPosition,
  loadPersistedModel,
  persistModel
} from './ModelPicker'

/**
 * P0-a 单测：验证「下拉面板被裁掉」这个根因被修掉。
 *
 * jsdom 不做真实布局，getBoundingClientRect 恒返回全 0，所以这里显式 mock
 * 触发按钮的 rect，并按面板 style 里声明的 fixed 定位/width/maxHeight 推导
 * 面板包围盒 —— 用 maxHeight 作为高度上界，断言的是「面板撑满也不会越出窗口」，
 * 比断言实际内容高度更严格。
 */

const MODELS: { id: string; owned_by?: string }[] = [
  { id: 'deepseek-chat', owned_by: 'deepseek' },
  { id: 'deepseek-reasoner', owned_by: 'deepseek' },
  { id: 'qwen-coder-32b', owned_by: 'alibaba' },
  { id: 'gpt-4o-mini', owned_by: 'openai' },
  { id: 'glm-4-air', owned_by: 'zhipu' },
  { id: 'moonshot-v1-8k', owned_by: 'moonshot' },
  { id: 'ernie-4.0', owned_by: 'baidu' },
  { id: 'spark-max', owned_by: 'iflytek' }
]

const VIEWPORT = { width: 1024, height: 768 }

type Rect = { top: number; bottom: number; left: number; right: number }

function stubListModels(payload: ModelListPayload): ReturnType<typeof vi.fn> {
  const listModels = vi.fn(async () => payload)
  window.ximo = { listModels } as unknown as XimoBridge
  return listModels
}

function mockRect(el: HTMLElement, rect: Rect): void {
  const full = {
    ...rect,
    width: rect.right - rect.left,
    height: rect.bottom - rect.top,
    x: rect.left,
    y: rect.top,
    toJSON: () => ({})
  }
  el.getBoundingClientRect = () => full as DOMRect
}

/** 从面板 inline style 推导包围盒（高度取 maxHeight 上界）。 */
function panelBox(panel: HTMLElement): { left: number; right: number; top: number; bottom: number } {
  const style = panel.style
  const width = Number.parseFloat(style.width)
  const height = Number.parseFloat(style.maxHeight)
  const left = Number.parseFloat(style.left)
  const top = style.top
    ? Number.parseFloat(style.top)
    : window.innerHeight - Number.parseFloat(style.bottom) - height
  return { left, right: left + width, top, bottom: top + height }
}

function expectInsideViewport(panel: HTMLElement): void {
  const box = panelBox(panel)
  expect(box.left).toBeGreaterThanOrEqual(0)
  expect(box.top).toBeGreaterThanOrEqual(0)
  expect(box.right).toBeLessThanOrEqual(window.innerWidth)
  expect(box.bottom).toBeLessThanOrEqual(window.innerHeight)
}

/** 打开面板并返回标题按钮与 Portal 面板节点。 */
async function openPanel(rect: Rect, value?: string): Promise<{ trigger: HTMLElement; panel: HTMLElement }> {
  const onChange = vi.fn()
  render(<ModelPicker value={value} onChange={onChange} />)
  const trigger = screen.getByTestId('model-picker-trigger')
  mockRect(trigger, rect)
  fireEvent.click(trigger)
  const panel = await screen.findByTestId('model-picker-panel')
  return { trigger, panel }
}

beforeEach(() => {
  Object.defineProperty(window, 'innerWidth', { value: VIEWPORT.width, configurable: true, writable: true })
  Object.defineProperty(window, 'innerHeight', { value: VIEWPORT.height, configurable: true, writable: true })
  window.localStorage.clear()
})

afterEach(() => {
  cleanup()
  window.localStorage.clear()
})

describe('computePanelPosition', () => {
  it('下方空间足够时向下展开，左右不越界', () => {
    const pos = computePanelPosition(
      { top: 100, bottom: 132, right: 880 },
      VIEWPORT
    )
    expect(pos.placeUp).toBe(false)
    expect(pos.style.top).toBe(140)
    expect(pos.style.bottom).toBeUndefined()
    expect(pos.style.left).toBe(580) // min(880-300, 1024-308)
    expect(pos.style.width).toBe(MODEL_PANEL_WIDTH)
    expect(pos.style.maxHeight).toBe(360)
  })

  it('贴近窗口底部时向上翻转', () => {
    const pos = computePanelPosition({ top: 700, bottom: 732, right: 880 }, VIEWPORT)
    expect(pos.placeUp).toBe(true)
    expect(pos.style.bottom).toBe(76) // 768 - 700 + 8
    expect(pos.style.top).toBeUndefined()
    expect(pos.style.maxHeight).toBe(360) // 上方 700px 足够
  })

  it('下方空间不足 260px 时也向上翻转', () => {
    const pos = computePanelPosition({ top: 600, bottom: 620, right: 880 }, VIEWPORT)
    expect(pos.placeUp).toBe(true)
  })

  it('左侧空间不足时不越出窗口左边界', () => {
    const pos = computePanelPosition({ top: 100, bottom: 132, right: 120 }, VIEWPORT)
    expect(pos.style.left).toBe(8)
  })
})

describe('ModelPicker 面板定位（Portal 到 body）', () => {
  it('向下展开：面板完整落在窗口内，且不在被裁的祖先容器里', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const { panel } = await openPanel({ top: 100, bottom: 132, left: 800, right: 880 })

    expect(panel.parentElement).toBe(document.body)
    expect(panel.dataset.place).toBe('down')
    expectInsideViewport(panel)
  })

  it('向上展开：面板完整落在窗口内', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const { panel } = await openPanel({ top: 700, bottom: 732, left: 800, right: 880 })

    expect(panel.parentElement).toBe(document.body)
    expect(panel.dataset.place).toBe('up')
    expectInsideViewport(panel)
  })

  it('窗口 resize 后重算位置', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const { trigger, panel } = await openPanel({ top: 100, bottom: 132, left: 800, right: 880 })
    expect(panel.dataset.place).toBe('down')

    // 触发按钮移动到窗口底部后 resize，应翻转为向上展开。
    mockRect(trigger, { top: 700, bottom: 732, left: 800, right: 880 })
    Object.defineProperty(window, 'innerHeight', { value: 768, configurable: true, writable: true })
    fireEvent(window, new Event('resize'))

    expect(screen.getByTestId('model-picker-panel').dataset.place).toBe('up')
    expectInsideViewport(screen.getByTestId('model-picker-panel'))
  })
})

describe('ModelPicker 搜索与键盘', () => {
  it('模型较多时显示搜索框，输入即过滤', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    await openPanel({ top: 100, bottom: 132, left: 800, right: 880 })

    expect(screen.getAllByRole('option')).toHaveLength(MODELS.length)

    const input = screen.getByLabelText('搜索模型')
    fireEvent.change(input, { target: { value: 'coder' } })

    const options = screen.getAllByRole('option')
    expect(options).toHaveLength(1)
    expect(options[0].textContent).toContain('qwen-coder-32b')

    // 按 owned_by 也能过滤
    fireEvent.change(input, { target: { value: 'zhipu' } })
    expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual([
      expect.stringContaining('glm-4-air')
    ])
  })

  it('无匹配时给出空态而不是空白', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    await openPanel({ top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.change(screen.getByLabelText('搜索模型'), { target: { value: '不存在的模型' } })

    expect(screen.queryAllByRole('option')).toHaveLength(0)
    expect(screen.getByText(/没有匹配/)).toBeTruthy()
  })

  it('↑↓ 移动高亮、Enter 选中并写入持久化', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const onChange = vi.fn()
    render(<ModelPicker value={undefined} onChange={onChange} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    await screen.findByTestId('model-picker-panel')

    const input = screen.getByLabelText('搜索模型')
    fireEvent.keyDown(input, { key: 'ArrowDown' })
    expect(screen.getAllByRole('option')[0].className).toContain('bg-surface')

    fireEvent.keyDown(input, { key: 'ArrowDown' })
    expect(screen.getAllByRole('option')[1].className).toContain('bg-surface')

    fireEvent.keyDown(input, { key: 'ArrowUp' })
    expect(screen.getAllByRole('option')[0].className).toContain('bg-surface')

    fireEvent.keyDown(input, { key: 'Enter' })
    expect(onChange).toHaveBeenCalledWith('deepseek-chat')
    expect(window.localStorage.getItem('ximo.model')).toBe('deepseek-chat')
    expect(screen.queryByTestId('model-picker-panel')).toBeNull()
  })

  it('Enter 未移动时不选择任何模型', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const onChange = vi.fn()
    render(<ModelPicker value={undefined} onChange={onChange} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    await screen.findByTestId('model-picker-panel')

    fireEvent.keyDown(screen.getByLabelText('搜索模型'), { key: 'Enter' })
    expect(onChange).not.toHaveBeenCalled()
    expect(screen.getByTestId('model-picker-panel')).toBeTruthy()
  })

  it('模型较少（无搜索框）时面板自行聚焦，键盘依旧可选', async () => {
    stubListModels({
      models: [{ id: 'alpha' }, { id: 'beta' }],
      base_url: 'http://localhost/v1'
    })
    const onChange = vi.fn()
    render(<ModelPicker value={undefined} onChange={onChange} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    const panel = await screen.findByTestId('model-picker-panel')

    expect(screen.queryByLabelText('搜索模型')).toBeNull()
    await waitFor(() => expect(document.activeElement).toBe(panel))

    fireEvent.keyDown(panel, { key: 'ArrowDown' })
    fireEvent.keyDown(panel, { key: 'ArrowDown' })
    fireEvent.keyDown(panel, { key: 'ArrowUp' })
    fireEvent.keyDown(panel, { key: 'Enter' })
    expect(onChange).toHaveBeenCalledWith('alpha')
  })

  it('Esc 关闭、点外部关闭、点面板内部不关闭', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const { panel } = await openPanel({ top: 100, bottom: 132, left: 800, right: 880 })

    // Portal 里的面板不属于触发按钮的 DOM 子树，点它不能算「外部点击」。
    fireEvent.mouseDown(screen.getByLabelText('搜索模型'))
    expect(screen.getByTestId('model-picker-panel')).toBe(panel)

    fireEvent.mouseDown(document.body)
    expect(screen.queryByTestId('model-picker-panel')).toBeNull()

    fireEvent.click(screen.getByTestId('model-picker-trigger'))
    await screen.findByTestId('model-picker-panel')
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(screen.queryByTestId('model-picker-panel')).toBeNull()
  })

  it('点击「跟随全局默认配置」回传 undefined', async () => {
    stubListModels({ models: MODELS, base_url: 'http://localhost/v1' })
    const onChange = vi.fn()
    render(<ModelPicker value="deepseek-chat" onChange={onChange} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    await screen.findByTestId('model-picker-panel')

    fireEvent.click(screen.getByText('跟随全局默认配置'))
    expect(onChange).toHaveBeenCalledWith(undefined)
    expect(window.localStorage.getItem('ximo.model')).toBeNull()
  })
})

describe('ModelPicker 异常/警告态', () => {
  it('value 不在当前服务商列表时显示警告色与提示，且不清空选择', async () => {
    stubListModels({ models: [{ id: 'deepseek-chat' }], base_url: 'http://localhost/v1' })
    const onChange = vi.fn()
    render(<ModelPicker value="ghost-model" onChange={onChange} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    await screen.findByTestId('model-picker-panel')

    expect(trigger.className).toContain('text-warning')
    expect(trigger.getAttribute('title')).toContain('该模型不在当前服务商列表中')
    expect(trigger.textContent).toContain('ghost-model')
    expect(onChange).not.toHaveBeenCalled()
  })

  it('列表拉取失败时不误报「不在列表中」', async () => {
    stubListModels({ models: null, base_url: '', error: 'boom' })
    render(<ModelPicker value="deepseek-chat" onChange={vi.fn()} />)
    const trigger = screen.getByTestId('model-picker-trigger')
    mockRect(trigger, { top: 100, bottom: 132, left: 800, right: 880 })
    fireEvent.click(trigger)
    await screen.findByTestId('model-picker-panel')

    expect(trigger.className).not.toContain('text-warning')
    expect(screen.getByText(/获取模型失败/)).toBeTruthy()
  })

  it('listModels 抛异常时降级为错误提示', async () => {
    const listModels = vi.fn(async () => {
      throw new Error('rpc down')
    })
    window.ximo = { listModels } as unknown as XimoBridge
    render(<ModelPicker value={undefined} onChange={vi.fn()} />)
    fireEvent.click(screen.getByTestId('model-picker-trigger'))
    await screen.findByTestId('model-picker-panel')

    expect(screen.getByText(/获取模型失败：rpc down/)).toBeTruthy()
  })
})

describe('模型持久化工具函数', () => {
  it('persistModel 写入，loadPersistedModel 读回', () => {
    expect(loadPersistedModel()).toBeUndefined()
    persistModel('deepseek-reasoner')
    expect(window.localStorage.getItem('ximo.model')).toBe('deepseek-reasoner')
    expect(loadPersistedModel()).toBe('deepseek-reasoner')

    persistModel(undefined)
    expect(window.localStorage.getItem('ximo.model')).toBeNull()
    expect(loadPersistedModel()).toBeUndefined()
  })

  it('空白值按「未选择」处理', () => {
    window.localStorage.setItem('ximo.model', '   ')
    expect(loadPersistedModel()).toBeUndefined()
  })

  it('localStorage 不可用时静默降级，不抛异常', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('access denied')
    })
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('access denied')
    })
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('access denied')
    })

    expect(loadPersistedModel()).toBeUndefined()
    expect(() => persistModel('x')).not.toThrow()
    expect(() => persistModel(undefined)).not.toThrow()
  })
})
