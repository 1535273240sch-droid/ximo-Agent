/**
 * 专家目录的数据层（v2.6.0）。
 *
 * 为什么单独一个 store 而不是复用 app-store：
 *   1. 目录是「内置 254 位 + 用户自定义」的一份远端清单，与「当前会话 / 当前
 *      run」无关。挂进 app-store 会让每次 run 事件都经过一份几百条的大数组比较。
 *   2. ExpertQuickPicker（输入框）与 ExpertsView（专家库页）都要用它。放进组件
 *      本地 state 就会出现「两处各存一份、保存后只有一处刷新」的经典分叉。
 *
 * 数据来源只有一个：window.ximo 的专家桥（经 setExpertsBridge 注入）。渲染层
 * 没有任何内置专家样本；单测用 setExpertsBridge 注入自己的假桥。
 *
 * 筛选/计数这类纯计算全部导出成纯函数，便于直接断言（不必渲染组件）。
 */

import { create } from 'zustand'
import type { ExpertCardPayload, ExpertDeletePayload, ExpertListPayload } from '@shared/types'
import { divisionLabel } from '../components/experts/experts-data'

/**
 * 专家库用到的桥接面。
 *
 * 只声明用到的三个方法，而不是直接引用 XimoBridge：单测里注入的假桥只需要实现
 * 这三个，不必伪造整个应用桥（伪造越少，测试越不容易因为无关改动而失效）。
 */
export interface ExpertsBridge {
  expertList(): Promise<ExpertListPayload>
  expertSave(card: ExpertCardPayload): Promise<ExpertCardPayload>
  expertDelete(id: string): Promise<ExpertDeletePayload>
}

/** 从全局 window.ximo 取桥；不存在时返回 undefined（单测环境没有 preload）。 */
export function bridgeFromWindow(): ExpertsBridge | undefined {
  if (typeof window === 'undefined') return undefined
  const w = window as unknown as { ximo?: Partial<ExpertsBridge> }
  const b = w.ximo
  if (!b || typeof b.expertList !== 'function') return undefined
  return b as ExpertsBridge
}

/** 模块级桥引用：不进 state（它不是渲染数据，进 state 会让每次 set 都做一次深比较）。 */
let bridge: ExpertsBridge | undefined

/**
 * 注入专家桥。
 *
 * 生产路径由 ExpertsView 挂载时注入 window.ximo；单测注入自己的假桥。
 * 传 undefined 可清空（组件卸载时调用，避免测试之间互相污染）。
 */
export function setExpertsBridge(b: ExpertsBridge | undefined): void {
  bridge = b
}

/** 取当前桥；没有桥时抛一个明确的错误，而不是让界面永远转圈。 */
export function requireExpertsBridge(): ExpertsBridge {
  const b = bridge ?? bridgeFromWindow()
  if (!b) {
    throw new Error('专家目录服务不可用：后端桥未就绪')
  }
  return b
}

/** 当前桥（可为空），供只读探测使用。 */
export function peekExpertsBridge(): ExpertsBridge | undefined {
  return bridge ?? bridgeFromWindow()
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}

// ---------------------------------------------------------------------------
// 纯函数：筛选 / 计数 / 工具清单
// ---------------------------------------------------------------------------

/** 专家筛选条件。三项都缺省时返回整个目录。 */
export interface ExpertFilter {
  /** 部门 slug；空字符串或 undefined 表示「全部部门」。 */
  division?: string
  /** 搜索词；在名称 / 描述 / 部门 slug / 部门中文名 / 执业风格上做大小写无关匹配。 */
  query?: string
  /** 只看用户自定义的专家。 */
  customOnly?: boolean
}

/** 纯函数：按部门 / 关键词 / 自定义标记过滤专家目录。 */
export function filterExperts(
  experts: ExpertCardPayload[],
  opts: ExpertFilter = {}
): ExpertCardPayload[] {
  const division = (opts.division ?? '').trim()
  const q = (opts.query ?? '').trim().toLowerCase()
  return experts.filter((e) => {
    if (division !== '' && e.division !== division) return false
    // custom 为 undefined 表示内置：只有显式 true 才算自定义。
    if (opts.customOnly && e.custom !== true) return false
    if (q === '') return true
    // 部门中文名也进搜索面：用户搜「界面设计」时不该因为目录里存的是 design 而落空。
    const hay = [
      e.name,
      e.description,
      e.division,
      divisionLabel(e.division),
      e.vibe ?? ''
    ]
      .join('\n')
      .toLowerCase()
    return hay.includes(q)
  })
}

/**
 * 纯函数：统计每个部门有多少位专家（只统计传入的这一份列表）。
 *
 * 筛选条上的数字必须等于点进去能看到的卡片数 —— 用后端给的全量数字去标注一个
 * 被过滤过的列表，就是 v2.5「工程研发 (58) 点开只有 6 张卡」那种谎话。
 */
export function divisionCounts(experts: ExpertCardPayload[]): Record<string, number> {
  const counts: Record<string, number> = {}
  for (const e of experts) {
    counts[e.division] = (counts[e.division] ?? 0) + 1
  }
  return counts
}

/**
 * 纯函数：目录里出现过的全部工具名（去重 + 排序）。
 *
 * 新建/编辑表单的勾选清单用它，而不是再抄一份工具表：后端已经按部门的真实推荐
 * 规则算好了每位专家的 tools，前端只需要把它们并起来给用户挑。
 *
 * 注意这是**候选**清单而非白名单：后端不校验工具名（运行时按本 build 的工具
 * 注册表过滤），所以表单另有一个自由输入框接受清单外的名字。
 */
export function toolCatalogue(experts: ExpertCardPayload[]): string[] {
  const set = new Set<string>()
  for (const e of experts) {
    for (const t of e.tools ?? []) {
      const name = t.trim()
      if (name !== '') set.add(name)
    }
  }
  return [...set].sort()
}

/**
 * 纯函数：归一化后端返回的部门清单。
 *
 * 后端一定会给 divisions，但一份空的 divisions 会让筛选条整个消失（用户无法按
 * 部门过滤），所以退化成「从专家自身推导」。顺序保持后端给的原序 —— 它反映目录
 * 的编排顺序，比字母序更有意义。
 */
export function normalizeDivisions(
  raw: string[] | undefined,
  experts: ExpertCardPayload[]
): string[] {
  const list = Array.isArray(raw)
    ? raw.filter((d): d is string => typeof d === 'string' && d.trim() !== '')
    : []
  if (list.length > 0) return list
  const derived: string[] = []
  for (const e of experts) {
    if (e.division !== '' && !derived.includes(e.division)) derived.push(e.division)
  }
  return derived
}

/** 后端说「没删掉」时的提示。deleted=false 只有两种可能：id 不存在，或是内置专家。 */
export const DELETE_MISS_MESSAGE =
  '删除失败：后端没有删除任何记录（该专家可能已被删除，或它属于不可删除的内置专家）'

// ---------------------------------------------------------------------------
// store
// ---------------------------------------------------------------------------

export interface ExpertsState {
  /** 完整目录：内置 + 用户自定义。 */
  experts: ExpertCardPayload[]
  /** 目录里出现的部门（来自后端）。 */
  divisions: string[]
  loading: boolean
  /** 最近一次失败的原因（中文；后端原文优先）。 */
  error?: string
  /** 是否成功装载过至少一次 —— 界面据此决定要不要懒加载。 */
  loaded: boolean

  /** 装载完整目录。失败时保留上一份列表（旧数据比空白有用），只把错误暴露出去。 */
  load: () => Promise<void>
  /** 新建或覆盖一位自定义专家；成功后重新装载目录并以服务端为准。 */
  save: (card: ExpertCardPayload) => Promise<ExpertCardPayload>
  /** 删除一位自定义专家；返回是否真的删掉了。 */
  remove: (id: string) => Promise<boolean>
}

export const useExpertsStore = create<ExpertsState>((set, get) => ({
  experts: [],
  divisions: [],
  loading: false,
  error: undefined,
  loaded: false,

  load: async () => {
    set({ loading: true, error: undefined })
    try {
      const b = requireExpertsBridge()
      const payload = await b.expertList()
      // experts 为 null 在契约里不该出现（后端显式兜底成 []），但这里是界面唯一
      // 的入口：一个 null 会让 .map 直接炸掉整个专家库页。
      const experts = Array.isArray(payload.experts) ? payload.experts : []
      set({
        experts,
        divisions: normalizeDivisions(payload.divisions, experts),
        loaded: true,
        loading: false
      })
    } catch (err) {
      // 保留旧列表：一次网络/进程抖动不该把用户已经看到的目录清空。
      set({ loading: false, error: `专家目录加载失败：${errText(err)}` })
    }
  },

  save: async (card) => {
    set({ error: undefined })
    try {
      const b = requireExpertsBridge()
      const saved = await b.expertSave(card)
      // 重新装载而不是本地插入/就地替换：id 由后端生成（空 id 会 slug 化）、
      // Custom 由后端强制、上级目录（divisions）也可能因此变化。前端猜一份，
      // 就会出现「界面显示了但库里没有」或重复卡片。
      await get().load()
      return saved
    } catch (err) {
      // 后端的中文校验信息（名称为空 / 覆盖内置专家 / 名称超长）原样透出，
      // 换成「保存失败」等于把用户唯一的修复线索抹掉。
      set({ error: errText(err) })
      throw err
    }
  },

  remove: async (id) => {
    set({ error: undefined })
    try {
      const b = requireExpertsBridge()
      const res = await b.expertDelete(id)
      // 无论删没删掉都刷新一次：本地列表可能已经过期（比如这条自定义专家在别处
      // 已被删掉），不刷新就会留下点不动的幽灵卡片。
      await get().load()
      if (res.deleted !== true) {
        // load() 成功时会清掉 error，所以这句必须写在刷新之后。
        // 用 ?? 而不是直接覆盖：如果刷新本身失败了，那个错误信息更值得显示。
        set((s) => ({ error: s.error ?? DELETE_MISS_MESSAGE }))
        return false
      }
      return true
    } catch (err) {
      set({ error: errText(err) })
      throw err
    }
  }
}))
