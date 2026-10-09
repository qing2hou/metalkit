import { ref } from 'vue'
import { describe, expect, it } from 'vitest'

import { usePagedTable } from './usePagedTable'

function makeItems(n: number): number[] {
  return Array.from({ length: n }, (_, i) => i)
}

describe('usePagedTable', () => {
  it('一页内不翻页：paged 返回全部且 pageCount=1', () => {
    const rows = ref(makeItems(15))
    const t = usePagedTable(rows)
    expect(t.paged.value).toHaveLength(15)
    expect(t.pageCount.value).toBe(1)
    expect(t.total.value).toBe(15)
  })

  it('默认每页 20：45 条切成 20/20/5', () => {
    const rows = ref(makeItems(45))
    const t = usePagedTable(rows)
    expect(t.pageCount.value).toBe(3)
    t.currentPage.value = 2
    expect(t.paged.value).toEqual(makeItems(45).slice(20, 40))
    t.currentPage.value = 3
    expect(t.paged.value).toEqual(makeItems(45).slice(40))
  })

  it('每页条数可切 50/100', () => {
    const rows = ref(makeItems(120))
    const t = usePagedTable(rows)
    t.pageSize.value = 50
    expect(t.pageCount.value).toBe(3)
    t.currentPage.value = 2
    expect(t.paged.value).toEqual(makeItems(120).slice(50, 100))
    t.pageSize.value = 100
    expect(t.pageCount.value).toBe(2)
  })

  it('数据刷新后当前页越界时回落到最后一页', () => {
    const rows = ref(makeItems(100))
    const t = usePagedTable(rows)
    t.currentPage.value = 5 // 最后一页
    rows.value = makeItems(30) // 刷新后只剩 30 条：2 页
    // 表格先读 paged（渲染），越界回落发生在这次读取里
    expect(t.paged.value).toHaveLength(10)
    expect(t.currentPage.value).toBe(2)
  })

  it('数据清空时回到第 1 页', () => {
    const rows = ref(makeItems(60))
    const t = usePagedTable(rows)
    t.currentPage.value = 3
    rows.value = []
    expect(t.paged.value).toHaveLength(0)
    expect(t.currentPage.value).toBe(1)
    expect(t.pageCount.value).toBe(1)
  })
})
