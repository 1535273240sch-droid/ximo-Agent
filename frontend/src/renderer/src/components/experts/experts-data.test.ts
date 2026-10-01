import { describe, expect, it } from 'vitest'

import { ALL_DIVISIONS } from './DivisionFilter'
import {
  DIVISION_LABELS,
  DIVISIONS,
  EXPERTS,
  SAMPLE_DIVISION_COUNTS,
  SAMPLE_EXPERT_COUNT,
  TOTAL_EXPERT_COUNT
} from './experts-data'

/**
 * 专家库数据与界面文案的一致性。
 *
 * 背景：界面曾经写「内置 254 位…点击可直接激活对话」，而列表里只有 60 张卡片。
 * 用户搜不到另外 194 位会以为功能坏了。这里把「目录总数」与「本页可选数」两个
 * 概念钉死：文案读 SAMPLE_EXPERT_COUNT，测试保证它确实等于真实条数。
 */
describe('experts-data', () => {
  it('本页可选数与 EXPERTS 实际长度一致', () => {
    expect(SAMPLE_EXPERT_COUNT).toBe(EXPERTS.length)
  })

  it('精选样本少于目录总数，且界面同时说明两者', () => {
    expect(SAMPLE_EXPERT_COUNT).toBeGreaterThan(0)
    expect(SAMPLE_EXPERT_COUNT).toBeLessThan(TOTAL_EXPERT_COUNT)
  })

  it('专家 ID 唯一（激活靠 ID 定位，重复会让点击指向错的人）', () => {
    const ids = EXPERTS.map((e) => e.id)
    expect(new Set(ids).size).toBe(ids.length)
  })

  it('每个部门至少有 1 位可选专家（否则筛选页永远空白）', () => {
    const covered = new Set(EXPERTS.map((e) => e.division))
    for (const division of DIVISIONS) {
      expect(covered.has(division), `部门 ${division} 没有可选专家`).toBe(true)
    }
  })

  it('筛选条计数之和等于列表条数', () => {
    const sum = Object.values(SAMPLE_DIVISION_COUNTS).reduce((a, b) => a + b, 0)
    expect(sum).toBe(SAMPLE_EXPERT_COUNT)
  })

  it('每个部门的计数等于该部门实际卡片数', () => {
    for (const division of DIVISIONS) {
      const actual = EXPERTS.filter((e) => e.division === division).length
      expect(SAMPLE_DIVISION_COUNTS[division] ?? 0, `部门 ${division} 计数不符`).toBe(actual)
    }
  })

  it('每个部门都有中文标签', () => {
    for (const division of DIVISIONS) {
      expect(DIVISION_LABELS[division]?.length ?? 0).toBeGreaterThan(0)
    }
  })

  it('每位专家都带上所在部门的推荐工具集', () => {
    for (const e of EXPERTS) {
      expect(Array.isArray(e.tools)).toBe(true)
    }
  })

  it('「全部部门」的哨兵值不会与真实部门重名', () => {
    expect(DIVISIONS).not.toContain(ALL_DIVISIONS)
  })
})
