import { ref, onMounted, onUnmounted } from 'vue'

/**
 * 通用轮询 composable：
 * - 页面隐藏（visibilitychange）自动暂停
 * - 暴露 remainingMs 供 UI 显示倒计时
 * - 再次可见时立刻刷新一次
 *
 * 与旧版行为一致：列表 30s、作业列表 5s、运行中作业 1s。
 */
export function usePolling(
  fn: () => Promise<void>,
  intervalMs: () => number,
  opts: { immediate?: boolean } = {},
) {
  const remainingMs = ref(0)
  const paused = ref(false)
  let timer: ReturnType<typeof setTimeout> | null = null
  let deadline = 0
  let countdownTimer: ReturnType<typeof setInterval> | null = null

  async function tick(): Promise<void> {
    try {
      await fn()
    } catch {
      // 轮询错误不打断循环；各视图自行决定是否展示错误
    }
    schedule()
  }

  function schedule(): void {
    clearTimer()
    if (paused.value) return
    const iv = Math.max(250, intervalMs())
    deadline = Date.now() + iv
    remainingMs.value = iv
    timer = setTimeout(tick, iv)
    countdownTimer = setInterval(() => {
      remainingMs.value = Math.max(0, deadline - Date.now())
    }, 500)
  }

  function clearTimer(): void {
    if (timer) clearTimeout(timer)
    if (countdownTimer) clearInterval(countdownTimer)
    timer = null
    countdownTimer = null
  }

  /** 手动触发一次立即刷新（重置轮询周期）。 */
  async function refresh(): Promise<void> {
    await tick()
  }

  function onVisibility(): void {
    if (document.hidden) {
      paused.value = true
      clearTimer()
    } else {
      paused.value = false
      // 恢复可见立刻刷一次，避免显示陈旧数据
      tick()
    }
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    if (opts.immediate !== false) {
      tick()
    } else {
      schedule()
    }
  })

  onUnmounted(() => {
    document.removeEventListener('visibilitychange', onVisibility)
    clearTimer()
  })

  return { remainingMs, paused, refresh }
}
