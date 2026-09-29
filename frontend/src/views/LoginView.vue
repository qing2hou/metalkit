<script setup lang="ts">
import type { FormInstance } from 'element-plus'
import { ElMessage } from 'element-plus'
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { authApi } from '@/api'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

const formRef = ref<FormInstance>()
const username = ref('')
const password = ref('')
const loading = ref(false)
const errorMsg = ref('')

const rules = {
  username: [{ required: true, message: '请输入用户名', trigger: 'blur' }],
  password: [{ required: true, message: '请输入密码', trigger: 'blur' }],
}

onMounted(() => {
  // 旧版 ?logout=1 语义：登出后跳回登录页给出提示
  if (route.query.logout === '1') {
    ElMessage.info('已退出登录')
    router.replace({ path: '/login' })
  }
})

async function submit(): Promise<void> {
  const valid = await formRef.value?.validate().catch(() => false)
  if (!valid) return
  loading.value = true
  errorMsg.value = ''
  try {
    const user = await authApi.login(username.value, password.value)
    auth.setUsername(user.username)
    const next = typeof route.query.next === 'string' ? route.query.next : '/'
    // next 只允许站内非登录路径（防开放跳转 & 防自指循环）
    const safe = next.startsWith('/') && !next.startsWith('/login') ? next : '/'
    router.replace(safe)
  } catch (err) {
    errorMsg.value = (err as Error).message || '登录失败'
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <main class="login-page">
    <el-card class="login-card">
      <h1 class="brand">metalkit</h1>
      <p class="muted">运维控制台</p>
      <el-alert v-if="errorMsg" :title="errorMsg" type="error" show-icon :closable="false" />
      <el-form
        ref="formRef"
        :model="{ username, password }"
        :rules="rules"
        label-position="top"
        @submit.prevent="submit"
      >
        <el-form-item label="用户名" prop="username">
          <el-input
            v-model="username"
            name="username"
            autocomplete="username"
            autofocus
            placeholder="用户名"
          />
        </el-form-item>
        <el-form-item label="密码" prop="password">
          <el-input
            v-model="password"
            name="password"
            type="password"
            autocomplete="current-password"
            show-password
            placeholder="密码"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" native-type="submit" class="login-submit" :loading="loading">
          登录
        </el-button>
      </el-form>
    </el-card>
  </main>
</template>

<style scoped>
.login-page {
  min-height: 100vh;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--el-bg-color-page);
}

.login-card {
  width: 360px;
  padding: 8px 4px;
}

.brand {
  font-size: 20px;
  font-weight: 700;
  margin: 4px 0 2px;
  text-align: left;
}

.muted {
  color: var(--el-text-color-secondary);
  margin: 0 0 16px;
  font-size: 13px;
}

.el-alert {
  margin-bottom: 12px;
}

.login-submit {
  width: 100%;
  margin-top: 4px;
}
</style>
