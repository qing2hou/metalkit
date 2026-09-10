import { ref } from 'vue'

/**
 * 轮询健康状态：连续失败时置 true，恢复后置 false。
 * 各页面用它显示常驻错误横幅（替代静默吞错）——值班场景下
 * “数据悄悄变旧”比“报错”危险得多。
 */
export function usePollHealth() {
  const unreachable = ref(false)
  let consecutiveFails = 0

  /** 在轮询 fetch 的 catch 里调用。 */
  function noteError(): void {
    consecutiveFails++
    if (consecutiveFails >= 2) unreachable.value = true
  }

  /** 在轮询 fetch 成功后调用。 */
  function noteOk(): void {
    consecutiveFails = 0
    unreachable.value = false
  }

  return { unreachable, noteError, noteOk }
}
