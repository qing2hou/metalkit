import { computed, ref, type Ref } from 'vue'

/**
 * 通用前端分页：全站表格统一「每页 20/50/100 可选，超过一页才显示分页器」。
 *
 * 数据规模为机房级（数百条内），列表 API 一次拉全量、这里纯前端切片，
 * 不给后端契约加负担；将来数据量上去需要服务端分页时，把 paged 换成
 * 请求参数即可，调用方组件不用改。
 *
 * 当前页的越界回落不依赖 watcher 的触发时机，而是在读取时钳制：
 * 数据刷新/每页条数变化后 currentPage 若超出最后一页，读取即回落。
 */
export function usePagedTable<T>(rows: Ref<T[]>, initial = 20) {
  const pageSize = ref(initial)
  const currentPage = ref(1)

  const total = computed(() => rows.value.length)
  const pageCount = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

  const paged = computed(() => {
    const maxPage = Math.max(1, Math.ceil(rows.value.length / pageSize.value))
    if (currentPage.value > maxPage) currentPage.value = maxPage
    const start = (currentPage.value - 1) * pageSize.value
    return rows.value.slice(start, start + pageSize.value)
  })

  return { pageSize, currentPage, total, pageCount, paged }
}
