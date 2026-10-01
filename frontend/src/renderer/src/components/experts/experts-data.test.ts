import { describe, expect, it } from 'vitest'

import { ALL_DIVISIONS } from './DivisionFilter'
import { DIVISIONS, DIVISION_LABELS, divisionLabel, tintColor } from './experts-data'

/**
 * 专家库「静态展示数据」的一致性。
 *
 * 背景：这里曾经还内联着 60 位专家样本，界面写着「共 254 位」却只渲染 60 张卡片
 * —— 用户搜不到另外 194 位会以为功能坏了。样本已删除，目录的唯一真相是后端；
 * 本文件因此只断言保留下来的展示层：部门中文名、未知部门的回退、颜色 tint。
 */
describe('experts-data 部门展示', () => {
  it('每个已翻译部门都有非空中文标签', () => {
    for (const division of DIVISIONS) {
      expect(DIVISION_LABELS[division]?.length ?? 0).toBeGreaterThan(0)
    }
  })

  it('divisionLabel 对未知部门回退到原始 slug（不能是 undefined/空）', () => {
    expect(divisionLabel('engineering')).toBe('工程研发')
    expect(divisionLabel('interface-design')).toBe('interface-design')
    expect(divisionLabel('')).toBe('')
  })

  it('DIVISIONS 由 DIVISION_LABELS 派生，不会漏掉任何一个已翻译部门', () => {
    expect([...DIVISIONS].sort()).toEqual(Object.keys(DIVISION_LABELS).sort())
  })

  it('「全部部门」的哨兵值不会与真实部门重名', () => {
    expect(DIVISIONS).not.toContain(ALL_DIVISIONS)
  })
})

describe('experts-data 颜色 tint', () => {
  it('#RRGGBB 直接追加透明度', () => {
    expect(tintColor('#3B82F6')).toBe('#3B82F622')
  })

  it('#RGB 先展开再追加（否则透明度会盖掉颜色分量）', () => {
    expect(tintColor('#abc')).toBe('#aabbcc22')
  })

  it('8 位 hex 只取前 6 位作颜色', () => {
    expect(tintColor('#12345678')).toBe('#12345622')
  })

  it('CSS 颜色名映射到 16 进制；未知名字与非法串回退到灰色', () => {
    expect(tintColor('blue')).toBe('#3B82F622')
    expect(tintColor('Teal')).toBe('#14B8A622')
    expect(tintColor('not-a-color')).toBe('#8A8A8A22')
    expect(tintColor('#12')).toBe('#8A8A8A22')
  })

  it('颜色为空（自定义专家可以不填）也产出合法 CSS，不抛异常', () => {
    expect(tintColor(undefined)).toBe('#8A8A8A22')
    expect(tintColor('')).toBe('#8A8A8A22')
    expect(tintColor('   ')).toBe('#8A8A8A22')
  })
})
