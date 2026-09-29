import { isProxy, isRef, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

/**
 * 过滤条件与 URL query 双向同步（对应旧版 list-filter.js / jobs.js 的行为）。
 * - 状态变化 → replace query（不产生历史记录）
 * - 路由 query 变化 → 回写状态（如浏览器后退）
 *
 * 同时接受 ref 与 reactive 对象：reactive 没有 .value，统一经 ref() 包装后
 * 两者在 watch 语义上一致（ref(reactive) 的 .value 即原代理）。
 */
export function useQuerySync<T extends Record<string, unknown>>(
  state: unknown,
  toQuery: (s: T) => Record<string, string>,
  fromQuery: (q: Record<string, string>) => Partial<T> | null,
): void {
  const route = useRoute()
  const router = useRouter()

  const get = (): T =>
    isRef<T>(state) ? state.value : (state as T)
  const set = (v: T): void => {
    if (isRef<T>(state)) state.value = v
    else Object.assign(state as T, v)
  }

  let syncing = false

  // query → state（初次挂载 & 浏览器导航）
  watch(
    () => route.query,
    (q) => {
      if (syncing) return
      const patch = fromQuery(q as Record<string, string>) ?? {}
      if (Object.keys(patch).length) {
        const next = { ...get(), ...patch }
        set(next)
      }
    },
    { immediate: true },
  )

  // state → query
  watch(
    () => get(),
    (s) => {
      if (syncing) return
      syncing = true
      const query = toQuery(s)
      router.replace({ query }).finally(() => {
        syncing = false
      })
    },
    { deep: true },
  )
}
