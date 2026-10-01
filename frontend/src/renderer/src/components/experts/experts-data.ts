/**
 * 专家库的**静态展示**数据（v2.6.0 之后）。
 *
 * 这里不再有任何专家样本。目录的唯一真相是后端：界面从 `store/experts-store`
 * 经 `window.ximo.expertList()` 取「内置 + 自定义」的完整目录，本模块只保留
 * 「怎么显示」这一层 —— 部门中文名与颜色 tint。
 *
 * 为什么把样本删干净而不是留着当兜底：v2.5 的坑正是「渲染层内联 60 位，而后端
 * 目录有 254 位」—— 界面写着 254 却只渲染 60 张卡片，用户搜不到其余专家会以为
 * 功能坏了。任何一份留在前端的专家副本都会再次变成这个谎言的源头。
 */

import type { ExpertCardPayload } from '@shared/types'

/**
 * 一位专家。
 *
 * 刻意做成后端载荷的别名而不是再声明一份结构：字段（emoji/color 可选、custom
 * 标记）完全由共享契约决定，两边各写一份只会在契约变更时静默漂移。
 */
export type Expert = ExpertCardPayload

/** 部门 → 中文展示名。slug 保持英文原值（与后端一致），只在界面上做本地化。 */
export const DIVISION_LABELS: Record<string, string> = {
  'engineering': '工程研发',
  'specialized': '专业领域',
  'marketing': '市场营销',
  'gis': '地理信息',
  'security': '安全合规',
  'design': '界面设计',
  'sales': '销售增长',
  'testing': '测试质量',
  'paid-media': '付费媒体',
  'project-management': '项目管理',
  'academic': '学术研究',
  'spatial-computing': '空间计算',
  'support': '客户支持',
  'finance': '财务金融',
  'game-development': '游戏开发',
  'product': '产品',
  'healthcare': '医疗健康'
}

/**
 * 已知部门的 slug 清单。
 *
 * 为什么还留着这个常量：设置页的「子代理模型分配」直接 import 它来列出尚未分配
 * 模型的部门，而本任务的改动范围不允许碰 `components/settings/**`（见任务书）
 * ——删掉它会让该面板编译不过。
 *
 * 它不是专家库页面的真相：筛选条一律使用后端返回的 `divisions`（那才可能包含
 * 用户自定义的新部门）。由 DIVISION_LABELS 派生而不是另抄一份数组，避免两份清单
 * 各自漂移。
 */
export const DIVISIONS: string[] = Object.keys(DIVISION_LABELS)

/**
 * 部门 slug → 中文展示名；未知部门回退到 slug 原文。
 *
 * 必须回退而不是返回 undefined：后端目录允许用户创建任意 slug 的新部门，界面上
 * 宁可显示 `my-team` 也不能显示空白或 "undefined"。
 */
export function divisionLabel(slug: string): string {
  return DIVISION_LABELS[slug] ?? slug
}

/** 非 16 进制颜色名的兜底色，保证 tint 永远产出合法 CSS。 */
const FALLBACK_HEX = '#8A8A8A'

/** 后端数据里出现的 CSS 颜色名 → 16 进制（仅用于生成 tint，不改动原始 color 字段）。 */
const NAMED_HEX: Record<string, string> = {
  blue: '#3B82F6',
  green: '#22C55E',
  purple: '#A855F7',
  teal: '#14B8A6',
  orange: '#F97316',
  amber: '#F59E0B',
  red: '#EF4444',
  pink: '#EC4899',
  indigo: '#6366F1',
  cyan: '#06B6D4',
  violet: '#8B5CF6',
  yellow: '#EAB308',
  slate: '#64748B',
  gold: '#C9A227',
  navy: '#1E3A5F',
  'metallic-blue': '#4A6FA5',
  'neon-cyan': '#22D3EE',
  'neon-green': '#39FF14'
}

/**
 * 由专家自身 color 生成低透明度 tint。
 *
 * 支持 #RGB / #RRGGBB / CSS 颜色名三种形态 —— 直接用 color + '22' 会在颜色名上
 * 产出 'blue22' 这种非法值，emoji 圆底就会整片透明。这是全组件唯一使用原始色值
 * 的位置（颜色是专家的真实数据，不属于主题令牌）。
 *
 * color 允许为空：专家目录里的 color 是可选的（自定义专家可以不填），调用方不必
 * 先做一遍判空；空值走 FALLBACK_HEX。
 */
export function tintColor(color: string | undefined, alpha = '22'): string {
  const raw = (color ?? '').trim()
  if (raw.startsWith('#')) {
    const hex = raw.slice(1)
    // #RGB → #RRGGBB，否则后两位透明度会盖掉颜色分量。
    const full =
      hex.length === 3
        ? hex
            .split('')
            .map((c) => c + c)
            .join('')
        : hex
    if (full.length === 6 || full.length === 8) return '#' + full.slice(0, 6) + alpha
    return FALLBACK_HEX + alpha
  }
  return (NAMED_HEX[raw.toLowerCase()] ?? FALLBACK_HEX) + alpha
}
