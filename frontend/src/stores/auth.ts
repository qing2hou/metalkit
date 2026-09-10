import { defineStore } from 'pinia'
import { ref } from 'vue'

import { authApi } from '@/api'

export const useAuthStore = defineStore('auth', () => {
  const username = ref<string>('')
  const loaded = ref(false)

  /** 应用启动时调用；未登录（401 由 client 统一跳转）时静默。 */
  async function fetchMe(): Promise<void> {
    try {
      const user = await authApi.me()
      username.value = user.username
    } catch {
      username.value = ''
    } finally {
      loaded.value = true
    }
  }

  function setUsername(name: string): void {
    username.value = name
  }

  async function logout(): Promise<void> {
    try {
      await authApi.logout()
    } finally {
      // 登录页带 ?logout=1 显示"已退出"提示
      window.location.assign('/ui/login?logout=1')
    }
  }

  return { username, loaded, fetchMe, setUsername, logout }
})
