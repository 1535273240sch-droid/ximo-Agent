import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ExpertCardPayload, ExpertListPayload } from '@shared/types'
import {
  DELETE_MISS_MESSAGE,
  divisionCounts,
  filterExperts,
  normalizeDivisions,
  peekExpertsBridge,
  setExpertsBridge,
  toolCatalogue,
  useExpertsStore,
  type ExpertsBridge
} from './experts-store'

/**
 * experts-store 的单测。
 *
 * 数据全部由本文件构造（渲染层不内置任何专家样本）。桥用 setExpertsBridge 注入，
 * 这样「装载 → 筛选 → 保存/删除 → 重新装载」的完整动作链都能被覆盖，不需要渲染
 * 任何组件。结构与 store/memory-store.test.ts 保持一致。
 */

function expert(partial: Partial<ExpertCardPayload> & { id: string }): ExpertCardPayload {
  return {
    division: 'engineering',
    name: partial.id,
    description: '',
    tools: [],
    ...partial
  }
}

const CATALOGUE: ExpertCardPayload[] = [
  expert({
    id: 'engineering-senior-developer',
    division: 'engineering',
    name: '高级开发工程师',
    description: '资深全栈开发工程师，精通架构设计与代码质量',
    vibe: '简单比复杂更难做到',
    tools: ['file_read', 'code_execute']
  }),
  expert({
    id: 'design-ui-designer',
    division: 'design',
    name: 'UI 设计师',
    description: '专精界面视觉设计与组件系统',
    vibe: '每个像素都在说话',
    tools: ['ui_generate', 'file_read']
  }),
  expert({
    id: 'custom-my-helper',
    division: 'engineering',
    name: '我的代码伙伴',
    description: '按我的团队规范做事',
    vibe: '先问清楚再动手',
    tools: ['file_write'],
    custom: true
  })
]

const LIST: ExpertListPayload = {
  experts: CATALOGUE,
  total: CATALOGUE.length,
  divisions: ['engineering', 'design']
}

function fakeBridge(overrides: Partial<ExpertsBridge> = {}): ExpertsBridge {
  return {
    expertList: vi.fn(async () => LIST),
    expertSave: vi.fn(async (card: ExpertCardPayload) => ({ ...card, id: card.id || 'custom-new' })),
    expertDelete: vi.fn(async () => ({ deleted: true })),
    ...overrides
  }
}

/** 重置 store 到初始状态（zustand 的 setState 支持部分更新）。 */
function resetStore(): void {
  useExpertsStore.setState({
    experts: [],
    divisions: [],
    loading: false,
    error: undefined,
    loaded: false
  })
}

beforeEach(() => {
  resetStore()
  setExpertsBridge(fakeBridge())
})

afterEach(() => {
  setExpertsBridge(undefined)
})

describe('experts-store 装载', () => {
  it('load 落地完整目录、部门与 loaded', async () => {
    const bridge = fakeBridge()
    setExpertsBridge(bridge)
    await useExpertsStore.getState().load()

    const s = useExpertsStore.getState()
    expect(bridge.expertList).toHaveBeenCalledTimes(1)
    expect(s.experts.map((e) => e.id)).toEqual([
      'engineering-senior-developer',
      'design-ui-designer',
      'custom-my-helper'
    ])
    // 界面靠 id 定位专家（激活、编辑、删除），重复 id 会让点击指向错的另一位。
    // 目录由后端合并，前端这一层只负责原样透传、不制造条目。
    expect(new Set(s.experts.map((e) => e.id)).size).toBe(s.experts.length)
    expect(s.divisions).toEqual(['engineering', 'design'])
    expect(s.loaded).toBe(true)
    expect(s.loading).toBe(false)
    expect(s.error).toBeUndefined()
  })

  it('load 失败时保留上一份列表并给出中文错误（不静默清空）', async () => {
    await useExpertsStore.getState().load()
    setExpertsBridge(
      fakeBridge({
        expertList: vi.fn(async () => {
          throw new Error('expert directory is not available in this build')
        })
      })
    )
    await useExpertsStore.getState().load()

    const s = useExpertsStore.getState()
    expect(s.error).toContain('专家目录加载失败')
    // 后端原文必须保留：它是排查「为什么目录空了」的唯一线索。
    expect(s.error).toContain('expert directory is not available in this build')
    expect(s.experts).toHaveLength(3)
    expect(s.loaded).toBe(true)
    expect(s.loading).toBe(false)
  })

  it('没有桥时给出明确错误，而不是永远 loading', async () => {
    setExpertsBridge(undefined)
    // 清掉 window.ximo（jsdom 里默认不存在）
    delete (window as unknown as { ximo?: unknown }).ximo
    expect(peekExpertsBridge()).toBeUndefined()

    await useExpertsStore.getState().load()
    const s = useExpertsStore.getState()
    expect(s.loading).toBe(false)
    expect(s.error).toContain('专家目录服务不可用')
  })

  it('后端没给 divisions 时从专家自身推导（否则筛选条会整个消失）', async () => {
    setExpertsBridge(
      fakeBridge({
        expertList: vi.fn(async () => ({ experts: CATALOGUE, total: 3, divisions: [] }))
      })
    )
    await useExpertsStore.getState().load()
    expect(useExpertsStore.getState().divisions).toEqual(['engineering', 'design'])
  })

  it('每个部门都至少有 1 位专家（否则点进去的筛选页永远空白）', async () => {
    await useExpertsStore.getState().load()
    const s = useExpertsStore.getState()
    for (const division of s.divisions) {
      expect(
        filterExperts(s.experts, { division }).length,
        `部门 ${division} 没有任何专家`
      ).toBeGreaterThan(0)
    }
  })
})

describe('experts-store 写操作', () => {
  it('save 先调 expertSave，再重新装载目录（以服务端为准，而不是本地拼接）', async () => {
    const bridge = fakeBridge()
    setExpertsBridge(bridge)
    const card = expert({ id: '', name: '新专家', division: 'design', custom: true })

    const saved = await useExpertsStore.getState().save(card)

    expect(bridge.expertSave).toHaveBeenCalledWith(card)
    // 关键：保存后必须再取一次目录 —— id 由后端生成、Custom 由后端强制。
    expect(bridge.expertList).toHaveBeenCalledTimes(1)
    expect(useExpertsStore.getState().experts).toHaveLength(3)
    expect(saved.id).toBe('custom-new')
  })

  it('save 把后端的中文校验错误原样抛出与显示（不替换成泛化文案）', async () => {
    const backendError =
      '专家 ID "design-ui-designer" 已属于内置专家，不能覆盖；请换一个 ID 或名称'
    setExpertsBridge(
      fakeBridge({
        expertSave: vi.fn(async () => {
          throw new Error(backendError)
        })
      })
    )

    await expect(
      useExpertsStore
        .getState()
        .save(expert({ id: 'design-ui-designer', name: 'UI 设计师', custom: true }))
    ).rejects.toThrow(backendError)
    expect(useExpertsStore.getState().error).toBe(backendError)
  })

  it('save 遇到「名称为空」这类后端错误也原样透出', async () => {
    setExpertsBridge(
      fakeBridge({
        expertSave: vi.fn(async () => {
          throw new Error('专家名称不能为空')
        })
      })
    )
    await expect(
      useExpertsStore.getState().save(expert({ id: '', name: '  ', custom: true }))
    ).rejects.toThrow('专家名称不能为空')
    expect(useExpertsStore.getState().error).toBe('专家名称不能为空')
  })

  it('remove 调 expertDelete 后重新装载，并返回 true', async () => {
    const bridge = fakeBridge()
    setExpertsBridge(bridge)

    await expect(useExpertsStore.getState().remove('custom-my-helper')).resolves.toBe(true)
    expect(bridge.expertDelete).toHaveBeenCalledWith('custom-my-helper')
    expect(bridge.expertList).toHaveBeenCalledTimes(1)
    expect(useExpertsStore.getState().error).toBeUndefined()
  })

  it('remove 在 deleted:false 时给出明确提示（id 不存在或属于内置专家）', async () => {
    const bridge = fakeBridge({
      expertDelete: vi.fn(async () => ({ deleted: false }))
    })
    setExpertsBridge(bridge)

    // 不抛异常：这不是「操作失败」，而是后端明确回答「没删掉」。
    await expect(useExpertsStore.getState().remove('engineering-senior-developer')).resolves.toBe(
      false
    )
    const s = useExpertsStore.getState()
    expect(s.error).toBe(DELETE_MISS_MESSAGE)
    expect(s.error).toContain('内置专家')
    // 即便没删掉也刷新了一次：本地列表可能已经过期。
    expect(bridge.expertList).toHaveBeenCalledTimes(1)
  })

  it('remove 的桥调用异常会让用户看到原因且不吞掉异常', async () => {
    setExpertsBridge(
      fakeBridge({
        expertDelete: vi.fn(async () => {
          throw new Error('专家 "x" 是内置专家，不能删除')
        })
      })
    )
    await expect(useExpertsStore.getState().remove('x')).rejects.toThrow('是内置专家，不能删除')
    expect(useExpertsStore.getState().error).toBe('专家 "x" 是内置专家，不能删除')
  })
})

describe('专家筛选与计数纯函数', () => {
  it('filterExperts 在名称 / 描述 / 部门 slug / 部门中文名 / 执业风格上匹配', () => {
    const ids = (qs: string): string[] =>
      filterExperts(CATALOGUE, { query: qs }).map((e) => e.id)

    expect(ids('高级')).toEqual(['engineering-senior-developer'])
    expect(ids('视觉')).toEqual(['design-ui-designer'])
    expect(ids('engineering')).toEqual(['engineering-senior-developer', 'custom-my-helper'])
    // 部门中文名：目录里存的是 design，用户搜「界面设计」也必须命中。
    expect(ids('界面设计')).toEqual(['design-ui-designer'])
    // vibe 也进搜索面。
    expect(ids('每个像素')).toEqual(['design-ui-designer'])
    // 大小写无关。
    expect(ids('UI')).toEqual(['design-ui-designer'])
    expect(ids('不存在的词')).toEqual([])
  })

  it('filterExperts 支持部门过滤与仅看自定义', () => {
    expect(filterExperts(CATALOGUE, { division: 'design' }).map((e) => e.id)).toEqual([
      'design-ui-designer'
    ])
    expect(filterExperts(CATALOGUE, { customOnly: true }).map((e) => e.id)).toEqual([
      'custom-my-helper'
    ])
    // 组合：筛选条 + 搜索词同时生效。
    expect(
      filterExperts(CATALOGUE, { division: 'engineering', query: '我的', customOnly: true }).map(
        (e) => e.id
      )
    ).toEqual(['custom-my-helper'])
    // 缺省参数与「全部部门」哨兵值（空字符串，见 DivisionFilter.ALL_DIVISIONS）都返回整份目录。
    expect(filterExperts(CATALOGUE)).toHaveLength(3)
    expect(filterExperts(CATALOGUE, { division: '' })).toHaveLength(3)
  })

  it('divisionCounts 按传入的那份列表计数（筛选条数字 = 点进去的卡片数）', () => {
    expect(divisionCounts(CATALOGUE)).toEqual({ engineering: 2, design: 1 })
    expect(divisionCounts(filterExperts(CATALOGUE, { customOnly: true }))).toEqual({
      engineering: 1
    })
    expect(divisionCounts([])).toEqual({})

    // 筛选条上的每一个数字都必须等于点进去能看到的卡片数，且总和等于列表条数
    // —— 这正是 v2.5「工程研发 (58) 点开只有 6 张卡」那个缺陷的反面。
    const counts = divisionCounts(CATALOGUE)
    const sum = Object.values(counts).reduce((a, b) => a + b, 0)
    expect(sum).toBe(CATALOGUE.length)
    for (const [division, count] of Object.entries(counts)) {
      expect(count).toBe(filterExperts(CATALOGUE, { division }).length)
    }
  })

  it('toolCatalogue 汇总全部工具名（去重、排序、忽略空串）', () => {
    expect(toolCatalogue(CATALOGUE)).toEqual([
      'code_execute',
      'file_read',
      'file_write',
      'ui_generate'
    ])
    expect(toolCatalogue([expert({ id: 'x', tools: ['b', ' b ', '', 'a'] })])).toEqual(['a', 'b'])
    expect(toolCatalogue([])).toEqual([])
    // tools 是可选字段（自定义专家可以不填）：缺了也要安全地当空列表处理。
    expect(toolCatalogue([expert({ id: 'no-tools', tools: undefined })])).toEqual([])
  })

  it('normalizeDivisions 保留后端原序，空值时按专家出现顺序推导', () => {
    expect(normalizeDivisions(['design', 'engineering'], CATALOGUE)).toEqual([
      'design',
      'engineering'
    ])
    expect(normalizeDivisions([], CATALOGUE)).toEqual(['engineering', 'design'])
    expect(normalizeDivisions(undefined, CATALOGUE)).toEqual(['engineering', 'design'])
  })
})
